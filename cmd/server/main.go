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