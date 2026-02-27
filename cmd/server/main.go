package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	
	"game-server/internal/config"
	"game-server/internal/logger"
	"game-server/pkg/anticheat"
	"game-server/pkg/auth"
	"game-server/pkg/compression"
	"game-server/pkg/game"
	"game-server/pkg/interest"
	"game-server/pkg/metrics"
	"game-server/pkg/network"
	"game-server/pkg/physics"
)

type GameServerApp struct {
	// Configuration
	config *config.Config
	logger *logger.Logger
	
	// Core systems
	gameServer      *game.Server
	matchManager    *game.MatchManager
	entityManager   *game.EntityManager
	
	// Network
	tcpServer       *network.TCPServer
	udpServer       *network.UDPServer
	connManager     *network.ConnectionManager
	packetHandler   *network.PacketHandler
	reliableUDP     *network.ReliableUDP
	
	// Game systems
	physicsSystem   *physics.System
	interestManager *interest.Manager
	deltaCompressor *compression.DeltaCompressor
	
	// Auth & Security
	authenticator   *auth.Authenticator
	sessionManager  *auth.SessionManager
	cheatDetector   *anticheat.CheatDetector
	
	// Monitoring
	metricsCollector *metrics.Collector
	prometheusExporter *metrics.PrometheusExporter
}

func main() {
	// Parse command line flags
	configPath := flag.String("config", "configs/server.yaml", "Path to config file")
	tcpPort := flag.String("tcp", "", "TCP port (overrides config)")
	udpPort := flag.String("udp", "", "UDP port (overrides config)")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	flag.Parse()
	
	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	
	// Override with flags
	if *tcpPort != "" {
		cfg.Network.TCPPort = *tcpPort
	}
	if *udpPort != "" {
		cfg.Network.UDPPort = *udpPort
	}
	
	// Initialize logger
	lgr := logger.New(*logLevel)
	lgr.Info("Starting game server...")
	
	// Create and initialize application
	app, err := NewGameServerApp(cfg, lgr)
	if err != nil {
		lgr.Fatal("Failed to initialize server: %v", err)
	}
	
	// Start all services
	if err := app.Start(); err != nil {
		lgr.Fatal("Failed to start server: %v", err)
	}
	
	lgr.Info("Game server started successfully")
	lgr.Info("TCP: %s, UDP: %s", cfg.Network.TCPPort, cfg.Network.UDPPort)
	
	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	
	lgr.Info("Shutting down...")
	
	// Graceful shutdown
	app.Stop()
	
	lgr.Info("Server stopped")
}

func NewGameServerApp(cfg *config.Config, lgr *logger.Logger) (*GameServerApp, error) {
	app := &GameServerApp{
		config: cfg,
		logger: lgr,
	}
	
	// Initialize metrics first
	app.metricsCollector = metrics.NewCollector()
	app.prometheusExporter = metrics.NewPrometheusExporter(app.metricsCollector)
	
	// Initialize game systems
	app.physicsSystem = physics.NewSystem(&physics.Config{
		Gravity:     cfg.Physics.Gravity,
		MaxSpeed:    cfg.Physics.MaxSpeed,
		GroundCheck: 0.1,
		CellSize:    100.0,
	})
	
	app.interestManager = interest.NewManager(
		cfg.Game.ViewDistance,
		100*time.Millisecond,
	)
	
	app.deltaCompressor = compression.NewDeltaCompressor()
	
	// Initialize match and entity management
	app.matchManager = game.NewMatchManager()
	app.entityManager = game.NewEntityManager()
	
	// Initialize game server
	app.gameServer = game.NewServer(&game.Config{
		TickRate:     cfg.Game.TickRate,
		ViewDistance: cfg.Game.ViewDistance,
	})
	app.gameServer.SetPhysicsSystem(app.physicsSystem)
	app.gameServer.SetInterestManager(app.interestManager)
	app.gameServer.SetDeltaCompressor(app.deltaCompressor)
	app.gameServer.SetMatchManager(app.matchManager)
	app.gameServer.SetEntityManager(app.entityManager)
	app.gameServer.SetMetricsCollector(app.metricsCollector)
	
	// Initialize auth systems
	app.authenticator = auth.NewAuthenticator(24 * time.Hour)
	app.sessionManager = auth.NewSessionManager(30 * time.Minute)
	
	// Initialize anti-cheat
	app.cheatDetector = anticheat.NewCheatDetector()
	app.gameServer.SetCheatDetector(app.cheatDetector)
	
	// Initialize network components
	app.connManager = network.NewConnectionManager(
		cfg.Network.TCPPort,
		cfg.Network.UDPPort,
	)
	app.connManager.SetAuthenticator(app.authenticator)
	app.connManager.SetSessionManager(app.sessionManager)
	
	// Create packet handler
	app.packetHandler = network.NewPacketHandler(app.gameServer)
	
	// Create network servers
	app.tcpServer = network.NewTCPServer(
		cfg.Network.TCPPort,
		app.connManager,
		app.gameServer,
	)
	
	app.udpServer = network.NewUDPServer(
		cfg.Network.UDPPort,
		app.connManager,
		app.gameServer,
	)
	app.udpServer.SetPacketHandler(app.packetHandler)
	
	// Initialize reliable UDP layer
	app.reliableUDP = network.NewReliableUDP(app.udpServer)
	app.udpServer.SetReliableLayer(app.reliableUDP)
	
	// Link game server to UDP server for sending packets
	app.gameServer.SetUDPServer(app.udpServer)
	
	return app, nil
}

func (app *GameServerApp) Start() error {
	// Start authentication cleanup
	go app.authenticator.CleanupExpiredTokens()
	go app.sessionManager.CleanupSessions()
	
	// Start anti-cheat cleanup
	go app.cheatDetector.GetValidator().CleanupOldData()
	
	// Start TCP server for authentication
	if err := app.tcpServer.Start(); err != nil {
		return err
	}
	
	// Start UDP server for game traffic
	if err := app.udpServer.Start(); err != nil {
		return err
	}
	
	// Start game server main loop
	app.gameServer.Start()
	
	// Start connection cleanup
	go app.connManager.CleanupSessions()
	
	// Start metrics collection
	app.metricsCollector.StartPeriodicSnapshot(5 * time.Second)
	app.metricsCollector.CalculatePacketRate()
	
	// Start Prometheus exporter
	app.prometheusExporter.StartPeriodicUpdate(5 * time.Second)
	go func() {
		if err := app.prometheusExporter.ServeHTTP(":9090"); err != nil {
			app.logger.Error("Prometheus server error: %v", err)
		}
	}()
	
	// Start periodic tasks
	go app.periodicTasks()
	
	return nil
}

func (app *GameServerApp) Stop() {
	// Stop network servers
	app.tcpServer.Stop()
	app.udpServer.Stop()
	
	// Stop game server
	app.gameServer.Stop()
	
	// Save any persistent data
	app.saveGameState()
	
	app.logger.Info("All services stopped")
}

func (app *GameServerApp) periodicTasks() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		// Update metrics
		app.updateMetrics()
		
		// Log server status
		app.logServerStatus()
		
		// Cleanup
		app.cleanup()
	}
}

func (app *GameServerApp) updateMetrics() {
	// Get player count
	playerCount := app.gameServer.GetPlayerCount()
	app.metricsCollector.UpdatePlayerCount(playerCount)
	
	// Get match count
	matches := app.matchManager.ListMatches()
	activeMatches := 0
	for _, match := range matches {
		if match.State == game.MatchStateInProgress {
			activeMatches++
		}
	}
	app.metricsCollector.UpdateMatchCount(activeMatches)
	
	// Update tick rate
	tickRate := app.gameServer.GetCurrentTickRate()
	app.metricsCollector.UpdateTickRate(tickRate)
}

func (app *GameServerApp) logServerStatus() {
	metrics := app.metricsCollector.GetCurrentMetrics()
	
	app.logger.Info("Status - Players: %d, Matches: %d, PPS: %d, Tick: %.1f, RTT: %dms",
		metrics.PlayerCount,
		metrics.ActiveMatches,
		metrics.PacketsPerSecond,
		metrics.TickRate,
		metrics.AverageRTT.Milliseconds(),
	)
}

func (app *GameServerApp) cleanup() {
	// Cleanup delta compressor
	app.deltaCompressor.Cleanup(5 * time.Minute)
	
	// Other cleanup tasks
}

func (app *GameServerApp) saveGameState() {
	// Save player data, match results, etc.
	app.logger.Info("Saving game state...")
}



func (egs *EnhancedGameServer) handleTCPConnection(conn net.Conn) {
	defer conn.Close()
	
	// Handle authentication
	session, err := egs.connectionManager.HandleTCPAuth(conn)
	if err != nil {
		log.Printf("Auth failed: %v", err)
		return
	}
	
	// Create player entry
	player := &Player{
		ID:        session.PlayerID,
		SessionID: session.SessionID,
		State: &pb.PlayerState{
			PlayerId: session.PlayerID,
			Position: &pb.Vector3{X: 0, Y: 0, Z: 0},
			Velocity: &pb.Vector3{X: 0, Y: 0, Z: 0},
			Rotation: &pb.Quaternion{X: 0, Y: 0, Z: 0, W: 1},
			Health:   100.0,
		},
		Connection: &PlayerConnection{
			LastPacket: time.Now(),
		},
	}
	
	egs.players.Store(session.PlayerID, player)
	
	log.Printf("Player %s ready for UDP connection", session.PlayerID)
	
	// Keep TCP connection alive for reliable messages
	// Handle chat, inventory, etc. here
	egs.handleReliableMessages(session, conn)
}

func (egs *EnhancedGameServer) startUDPListener() {
	addr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf(":%s", egs.connectionManager.udpPort))
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	
	egs.udpConn = conn
	conn.SetReadBuffer(1024 * 1024)
	conn.SetWriteBuffer(1024 * 1024)
	
	log.Printf("UDP Game server listening on :%s", egs.connectionManager.udpPort)
	
	buffer := make([]byte, 1400)
	
	for {
		n, addr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}
		
		go egs.handleUDPPacket(buffer[:n], addr)
	}
}

func (egs *EnhancedGameServer) handleUDPPacket(data []byte, addr *net.UDPAddr) {
	// Try to get existing session
	session, exists := egs.connectionManager.GetSessionByUDP(addr)
	
	if !exists {
		// This might be a handshake packet
		session, err := egs.connectionManager.HandleUDPHandshake(data, addr)
		if err != nil {
			log.Printf("UDP handshake failed from %s: %v", addr, err)
			return
		}
		
		// Send handshake acknowledgment
		egs.sendUDPHandshakeAck(addr, session)
		return
	}
	
	// Parse game packet
	packet := &pb.GamePacket{}
	if err := proto.Unmarshal(data, packet); err != nil {
		return
	}
	
	// Process packet based on type
	switch packet.Type {
	case pb.PacketType_INPUT:
		egs.handlePlayerInput(session, packet.GetInput())
	case pb.PacketType_PING:
		egs.handlePing(session, addr)
	}
}

// Enhanced broadcast with delta compression and interest management
func (egs *EnhancedGameServer) broadcastGameState() {
	// Collect all entities for spatial indexing
	entities := make(map[string]*Entity)
	egs.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		entities[player.ID] = &Entity{
			ID:       player.ID,
			Position: player.State.Position,
			Radius:   0.5,
		}
		return true
	})
	
	// Update spatial index for interest management
	egs.interestManager.UpdateSpatialIndex(entities)
	
	// Broadcast to each player
	egs.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		
		// Update player's area of interest
		egs.interestManager.UpdateInterest(player.ID, player.State.Position)
		
		// Get entities player is interested in
		interestedEntities := egs.interestManager.GetInterestedEntities(player.ID)
		
		// Build delta update only for interested entities
		deltas := make([]*pb.DeltaUpdate, 0, len(interestedEntities))
		
		for _, entityID := range interestedEntities {
			if otherPlayer, ok := egs.players.Load(entityID); ok {
				p := otherPlayer.(*Player)
				
				// Compute delta
				delta := egs.deltaCompressor.ComputeDelta(entityID, p.State)
				
				// Only send if something changed
				if delta.HasChanges() {
					deltas = append(deltas, delta.ToProto())
				}
			}
		}
		
		// Don't send empty updates
		if len(deltas) == 0 {
			return true
		}
		
		// Create delta world state
		deltaState := &pb.DeltaWorldState{
			SnapshotId: egs.serverTick,
			Deltas:     deltas,
			ServerTick: egs.serverTick,
		}
		
		// Send via UDP
		egs.sendDeltaUpdate(player, deltaState)
		
		return true
	})
}

func (egs *EnhancedGameServer) sendDeltaUpdate(player *Player, deltaState *pb.DeltaWorldState) {
	if player.Connection.UDPAddr == nil {
		return
	}
	
	data, err := proto.Marshal(deltaState)
	if err != nil {
		return
	}
	
	egs.udpConn.WriteToUDP(data, player.Connection.UDPAddr)
}

func (egs *EnhancedGameServer) cleanupDeltaCompressor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		egs.deltaCompressor.Cleanup(5 * time.Minute)
	}
}

func (egs *EnhancedGameServer) handleReliableMessages(session *Session, conn net.Conn) {
	// Handle reliable TCP messages (chat, inventory, etc.)
	// Keep connection open for bidirectional communication
	
	buffer := make([]byte, 4096)
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		
		n, err := conn.Read(buffer)
		if err != nil {
			log.Printf("TCP read error for session %s: %v", session.SessionID, err)
			egs.connectionManager.DisconnectSession(session.SessionID)
			return
		}
		
		// Process reliable message
		// Parse and handle chat, inventory updates, etc.
		_ = n
	}
}

func (egs *EnhancedGameServer) sendUDPHandshakeAck(addr *net.UDPAddr, session *Session) {
	ack := &pb.UDPHandshakeAck{
		Success:    true,
		ServerTick: egs.serverTick,
	}
	
	data, _ := proto.Marshal(ack)
	egs.udpConn.WriteToUDP(data, addr)
}

func (egs *EnhancedGameServer) handlePlayerInput(session *Session, input *pb.InputUpdate) {
	playerVal, exists := egs.players.Load(session.PlayerID)
	if !exists {
		return
	}
	
	player := playerVal.(*Player)
	player.LastInput = input
}

func (egs *EnhancedGameServer) handlePing(session *Session, addr *net.UDPAddr) {
	pong := &pb.GamePacket{
		Type:    pb.PacketType_PONG,
		Payload: &pb.GamePacket_Pong{Pong: &pb.Pong{}},
	}
	
	data, _ := proto.Marshal(pong)
	egs.udpConn.WriteToUDP(data, addr)
}