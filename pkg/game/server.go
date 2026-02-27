package game

import (
	"log"
	"sync"
	"time"
	
	"game-server/pkg/anticheat"
	"game-server/pkg/compression"
	"game-server/pkg/interest"
	"game-server/pkg/metrics"
	"game-server/pkg/network"
	"game-server/pkg/physics"
	pb "game-server/proto"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	config *Config
	
	// Systems
	physics         *physics.System
	interestManager *interest.Manager
	deltaCompressor *compression.DeltaCompressor
	matchManager    *MatchManager
	entityManager   *EntityManager
	cheatDetector   *anticheat.CheatDetector
	metricsCollector *metrics.Collector
	
	// State
	players    sync.Map
	serverTick uint64
	tickRate   int
	running    bool
	
	// Network
	udpServer *network.UDPServer
	
	// Channels
	inputCh    chan *PlayerInput
	commandCh  chan *PlayerCommand
	stopCh     chan struct{}
	
	// State history for lag compensation
	stateHistory *StateHistory
	
	mu sync.RWMutex
}

type PlayerInput struct {
	SessionID string
	PlayerID  string
	Input     *pb.InputUpdate
}

type PlayerCommand struct {
	PlayerID string
	Command  string
	Data     interface{}
}

type StateHistory struct {
	states   []*HistoricalState
	maxSize  int
	mu       sync.RWMutex
}

type HistoricalState struct {
	Tick      uint64
	Timestamp time.Time
	Players   map[string]*pb.PlayerState
}

func NewServer(config *Config) *Server {
	return &Server{
		config:       config,
		inputCh:      make(chan *PlayerInput, 1000),
		commandCh:    make(chan *PlayerCommand, 100),
		stopCh:       make(chan struct{}),
		tickRate:     config.TickRate,
		stateHistory: NewStateHistory(300), // 5 seconds at 60Hz
	}
}

// Setters for dependency injection
func (s *Server) SetPhysicsSystem(ps *physics.System) {
	s.physics = ps
}

func (s *Server) SetInterestManager(im *interest.Manager) {
	s.interestManager = im
}

func (s *Server) SetDeltaCompressor(dc *compression.DeltaCompressor) {
	s.deltaCompressor = dc
}

func (s *Server) SetMatchManager(mm *MatchManager) {
	s.matchManager = mm
}

func (s *Server) SetEntityManager(em *EntityManager) {
	s.entityManager = em
}

func (s *Server) SetCheatDetector(cd *anticheat.CheatDetector) {
	s.cheatDetector = cd
}

func (s *Server) SetMetricsCollector(mc *metrics.Collector) {
	s.metricsCollector = mc
}

func (s *Server) SetUDPServer(udpServer *network.UDPServer) {
	s.udpServer = udpServer
}

func (s *Server) Start() {
	s.running = true
	
	go s.gameLoop()
	go s.processInputs()
	go s.processCommands()
	
	log.Println("Game server started")
}

func (s *Server) Stop() {
	s.running = false
	close(s.stopCh)
	
	log.Println("Game server stopped")
}

func (s *Server) gameLoop() {
	ticker := time.NewTicker(time.Second / time.Duration(s.tickRate))
	defer ticker.Stop()
	
	lastTick := time.Now()
	
	for s.running {
		select {
		case <-ticker.C:
			now := time.Now()
			deltaTime := now.Sub(lastTick).Seconds()
			lastTick = now
			
			s.serverTick++
			
			// Update physics
			s.updatePhysics(float32(deltaTime))
			
			// Update entities
			if s.entityManager != nil {
				s.entityManager.UpdateAll(float32(deltaTime))
			}
			
			// Update matches
			s.updateMatches(float32(deltaTime))
			
			// Update game state
			s.updateGameState()
			
			// Record state for lag compensation
			s.recordState()
			
			// Broadcast snapshots (every other tick = 30Hz at 60Hz tick)
			if s.serverTick%2 == 0 {
				s.broadcastState()
			}
			
			// Periodic cleanup
			if s.serverTick%600 == 0 { // Every 10 seconds
				s.periodicCleanup()
			}
			
		case <-s.stopCh:
			return
		}
	}
}

func (s *Server) processInputs() {
	for input := range s.inputCh {
		s.handlePlayerInputInternal(input)
	}
}

func (s *Server) processCommands() {
	for cmd := range s.commandCh {
		s.handlePlayerCommand(cmd)
	}
}

func (s *Server) updatePhysics(deltaTime float32) {
	if s.physics == nil {
		return
	}
	
	s.physics.Step(float64(deltaTime))
}

func (s *Server) updateMatches(deltaTime float32) {
	if s.matchManager == nil {
		return
	}
	
	matches := s.matchManager.ListMatches()
	for _, match := range matches {
		match.Update(deltaTime)
	}
}

func (s *Server) updateGameState() {
	// Update any global game state
}

func (s *Server) recordState() {
	if s.stateHistory == nil {
		return
	}
	
	// Collect all player states
	playerStates := make(map[string]*pb.PlayerState)
	
	s.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		playerStates[player.ID] = proto.Clone(player.State).(*pb.PlayerState)
		return true
	})
	
	s.stateHistory.Record(s.serverTick, playerStates)
}

func (s *Server) broadcastState() {
	// Collect entities for spatial indexing
	entities := make(map[string]*physics.Entity)
	
	s.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		entities[player.ID] = &physics.Entity{
			ID:       player.ID,
			Position: player.State.Position,
			Radius:   0.5,
		}
		return true
	})
	
	// Update spatial index
	if s.interestManager != nil {
		s.interestManager.UpdateSpatialIndex(entities)
	}
	
	// Broadcast to each player
	s.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		s.broadcastToPlayer(player)
		return true
	})
}

func (s *Server) broadcastToPlayer(player *Player) {
	if player.Connection.UDPAddr == nil {
		return
	}
	
	// Update interest
	if s.interestManager != nil {
		s.interestManager.UpdateInterest(player.ID, player.State.Position)
	}
	
	// Get interested entities
	interestedEntities := []string{}
	if s.interestManager != nil {
		interestedEntities = s.interestManager.GetInterestedEntities(player.ID)
	} else {
		// If no interest manager, send all players
		s.players.Range(func(key, value interface{}) bool {
			p := value.(*Player)
			if p.ID != player.ID {
				interestedEntities = append(interestedEntities, p.ID)
			}
			return true
		})
	}
	
	// Build delta updates
	deltas := make([]*pb.DeltaUpdate, 0)
	
	for _, entityID := range interestedEntities {
		if otherPlayer, ok := s.players.Load(entityID); ok {
			p := otherPlayer.(*Player)
			
			var delta *compression.DeltaState
			if s.deltaCompressor != nil {
				delta = s.deltaCompressor.ComputeDelta(entityID, p.State)
			} else {
				// No compression, send full state
				delta = &compression.DeltaState{
					PlayerID:         entityID,
					PositionChanged:  true,
					Position:         p.State.Position,
					VelocityChanged:  true,
					Velocity:         p.State.Velocity,
					RotationChanged:  true,
					Rotation:         p.State.Rotation,
					HealthChanged:    true,
					Health:           p.State.Health,
					AnimationChanged: true,
					AnimationState:   p.State.AnimationState,
					Timestamp:        uint64(time.Now().UnixNano()),
				}
			}
			
			if delta.HasChanges() {
				deltas = append(deltas, delta.ToProto())
			}
		}
	}
	
	// Don't send empty updates
	if len(deltas) == 0 {
		return
	}
	
	// Create delta world state
	deltaState := &pb.DeltaWorldState{
		SnapshotId: s.serverTick,
		Deltas:     deltas,
		ServerTick: s.serverTick,
	}
	
	// Send via UDP
	s.sendDeltaUpdate(player, deltaState)
	
	// Record metrics
	if s.metricsCollector != nil {
		data, _ := proto.Marshal(deltaState)
		s.metricsCollector.RecordPacket(true, uint64(len(data)))
	}
}

func (s *Server) sendDeltaUpdate(player *Player, deltaState *pb.DeltaWorldState) {
	if s.udpServer == nil {
		return
	}
	
	data, err := proto.Marshal(deltaState)
	if err != nil {
		return
	}
	
	s.udpServer.SendPacket(player.Connection.UDPAddr, data)
}

func (s *Server) handlePlayerInputInternal(input *PlayerInput) {
	playerVal, exists := s.players.Load(input.PlayerID)
	if !exists {
		return
	}
	
	player := playerVal.(*Player)
	
	// Validate input with anti-cheat
	if s.cheatDetector != nil {
		validator := s.cheatDetector.GetValidator()
		
		if !validator.ValidateInput(player.ID, input.Input) {
			log.Printf("Invalid input from player %s", player.ID)
			return
		}
		
		// Validate movement if we have previous state
		if player.LastInput != nil {
			deltaTime := float32(1.0 / float64(s.tickRate))
			predictedState := s.physics.ApplyInput(player.State, input.Input, deltaTime)
			
			if !validator.ValidateMovement(player.ID, player.State, predictedState, deltaTime) {
				log.Printf("Invalid movement from player %s", player.ID)
				
				// Check if player should be kicked
				if validator.ShouldKickPlayer(player.ID) {
					s.kickPlayer(player.ID, "Suspicious activity detected")
				}
				return
			}
		}
	}
	
	// Apply physics
	deltaTime := float32(1.0 / float64(s.tickRate))
	newState := s.physics.ApplyInput(player.State, input.Input, deltaTime)
	
	player.State = newState
	player.LastInput = input.Input
}

func (s *Server) handlePlayerCommand(cmd *PlayerCommand) {
	// Handle various player commands
	switch cmd.Command {
	case "respawn":
		s.respawnPlayer(cmd.PlayerID)
	case "use_item":
		// Handle item usage
	default:
		log.Printf("Unknown command: %s", cmd.Command)
	}
}

func (s *Server) periodicCleanup() {
	// Cleanup tasks
	if s.deltaCompressor != nil {
		s.deltaCompressor.Cleanup(5 * time.Minute)
	}
}

// Public interface methods

func (s *Server) OnPlayerConnected(session *network.Session) {
	player := NewPlayer(session.PlayerID, session.SessionID)
	s.players.Store(session.PlayerID, player)
	
	log.Printf("Player %s connected", session.PlayerID)
	
	// Update metrics
	if s.metricsCollector != nil {
		s.metricsCollector.UpdatePlayerCount(s.GetPlayerCount())
	}
}

func (s *Server) OnPlayerDisconnected(playerID string) {
	s.players.Delete(playerID)
	
	// Remove from interest manager
	if s.interestManager != nil {
		s.interestManager.RemovePlayer(playerID)
	}
	
	log.Printf("Player %s disconnected", playerID)
	
	// Update metrics
	if s.metricsCollector != nil {
		s.metricsCollector.UpdatePlayerCount(s.GetPlayerCount())
	}
}

func (s *Server) HandleGamePacket(session *network.Session, packet *pb.GamePacket) {
	switch packet.Type {
	case pb.PacketType_INPUT:
		s.inputCh <- &PlayerInput{
			SessionID: session.SessionID,
			PlayerID:  session.PlayerID,
			Input:     packet.GetInput(),
		}
		
	case pb.PacketType_COMMAND:
		// Handle commands
	}
}

func (s *Server) HandlePlayerInput(sessionID string, input *pb.InputUpdate) {
	// Find player by session
	var playerID string
	s.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		if player.SessionID == sessionID {
			playerID = player.ID
			return false
		}
		return true
	})
	
	if playerID != "" {
		s.inputCh <- &PlayerInput{
			SessionID: sessionID,
			PlayerID:  playerID,
			Input:     input,
		}
	}
}

func (s *Server) HandlePlayerDisconnect(sessionID string) {
	// Find and remove player
	var playerID string
	s.players.Range(func(key, value interface{}) bool {
		player := value.(*Player)
		if player.SessionID == sessionID {
			playerID = player.ID
			return false
		}
		return true
	})
	
	if playerID != "" {
		s.OnPlayerDisconnected(playerID)
	}
}

func (s *Server) HandlePing(sessionID string) time.Duration {
	// Calculate and return RTT
	return 50 * time.Millisecond // Placeholder
}

func (s *Server) GetPlayerCount() int {
	count := 0
	s.players.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

func (s *Server) GetCurrentTickRate() float64 {
	return float64(s.tickRate)
}

func (s *Server) kickPlayer(playerID, reason string) {
	log.Printf("Kicking player %s: %s", playerID, reason)
	s.OnPlayerDisconnected(playerID)
}

func (s *Server) respawnPlayer(playerID string) {
	playerVal, exists := s.players.Load(playerID)
	if !exists {
		return
	}
	
	player := playerVal.(*Player)
	
	// Reset player state
	player.State.Position.X = 0
	player.State.Position.Y = 0
	player.State.Position.Z = 0
	player.State.Health = 100
	player.State.Velocity.X = 0
	player.State.Velocity.Y = 0
	player.State.Velocity.Z = 0
}

func NewStateHistory(maxSize int) *StateHistory {
	return &StateHistory{
		states:  make([]*HistoricalState, 0, maxSize),
		maxSize: maxSize,
	}
}

func (sh *StateHistory) Record(tick uint64, players map[string]*pb.PlayerState) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	
	state := &HistoricalState{
		Tick:      tick,
		Timestamp: time.Now(),
		Players:   players,
	}
	
	sh.states = append(sh.states, state)
	
	if len(sh.states) > sh.maxSize {
		sh.states = sh.states[1:]
	}
}

func (sh *StateHistory) GetStateAt(tick uint64) *HistoricalState {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	
	for i := len(sh.states) - 1; i >= 0; i-- {
		if sh.states[i].Tick <= tick {
			return sh.states[i]
		}
	}
	
	return nil
}