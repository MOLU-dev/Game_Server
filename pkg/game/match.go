package game

import (
	"fmt"
	"sync"
	"time"
)

type Match struct {
	ID          string
	Name        string
	MapName     string
	Mode        MatchMode
	State       MatchState
	MaxPlayers  int
	Players     []*Player
	Teams       map[uint32]*Team
	StartTime   time.Time
	EndTime     time.Time
	Duration    time.Duration
	Score       map[uint32]int
	mu          sync.RWMutex
}

type MatchMode int

const (
	MatchModeDeathmatch MatchMode = iota
	MatchModeTeamDeathmatch
	MatchModeCaptureTheFlag
	MatchModeBattleRoyale
	MatchModeObjective
)

type MatchState int

const (
	MatchStateWaiting MatchState = iota
	MatchStateStarting
	MatchStateInProgress
	MatchStateEnding
	MatchStateEnded
)

type Team struct {
	ID      uint32
	Name    string
	Color   string
	Players []*Player
	Score   int
}

type MatchManager struct {
	matches map[string]*Match
	mu      sync.RWMutex
}

func NewMatchManager() *MatchManager {
	return &MatchManager{
		matches: make(map[string]*Match),
	}
}

// CreateMatch creates a new match
func (mm *MatchManager) CreateMatch(config *MatchConfig) *Match {
	match := &Match{
		ID:         generateMatchID(),
		Name:       config.Name,
		MapName:    config.MapName,
		Mode:       config.Mode,
		State:      MatchStateWaiting,
		MaxPlayers: config.MaxPlayers,
		Players:    make([]*Player, 0, config.MaxPlayers),
		Teams:      make(map[uint32]*Team),
		Score:      make(map[uint32]int),
		Duration:   config.Duration,
	}
	
	// Create teams if team-based mode
	if config.Mode == MatchModeTeamDeathmatch || config.Mode == MatchModeCaptureTheFlag {
		match.Teams[1] = &Team{ID: 1, Name: "Red Team", Color: "#FF0000"}
		match.Teams[2] = &Team{ID: 2, Name: "Blue Team", Color: "#0000FF"}
	}
	
	mm.mu.Lock()
	mm.matches[match.ID] = match
	mm.mu.Unlock()
	
	return match
}

type MatchConfig struct {
	Name       string
	MapName    string
	Mode       MatchMode
	MaxPlayers int
	Duration   time.Duration
}

// AddPlayer adds a player to the match
func (m *Match) AddPlayer(player *Player) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if len(m.Players) >= m.MaxPlayers {
		return fmt.Errorf("match is full")
	}
	
	if m.State != MatchStateWaiting && m.State != MatchStateStarting {
		return fmt.Errorf("match already in progress")
	}
	
	m.Players = append(m.Players, player)
	player.MatchID = m.ID
	
	// Assign to team if team-based
	if len(m.Teams) > 0 {
		m.assignPlayerToTeam(player)
	}
	
	// Start match if enough players
	if len(m.Players) >= m.MaxPlayers/2 && m.State == MatchStateWaiting {
		m.State = MatchStateStarting
	}
	
	return nil
}

// RemovePlayer removes a player from the match
func (m *Match) RemovePlayer(playerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	for i, player := range m.Players {
		if player.ID == playerID {
			m.Players = append(m.Players[:i], m.Players[i+1:]...)
			player.MatchID = ""
			
			// Remove from team
			if team, ok := m.Teams[player.Team]; ok {
				for j, p := range team.Players {
					if p.ID == playerID {
						team.Players = append(team.Players[:j], team.Players[j+1:]...)
						break
					}
				}
			}
			
			return nil
		}
	}
	
	return fmt.Errorf("player not in match")
}

// Start starts the match
func (m *Match) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if m.State != MatchStateStarting {
		return fmt.Errorf("match not ready to start")
	}
	
	m.State = MatchStateInProgress
	m.StartTime = time.Now()
	
	// Spawn all players
	for _, player := range m.Players {
		m.spawnPlayer(player)
	}
	
	return nil
}

// End ends the match
func (m *Match) End() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.State = MatchStateEnded
	m.EndTime = time.Now()
	
	// Calculate final scores, awards, etc.
}

// Update updates match state (called every tick)
func (m *Match) Update(deltaTime float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	switch m.State {
	case MatchStateStarting:
		// Countdown logic
		
	case MatchStateInProgress:
		// Check win conditions
		if time.Since(m.StartTime) >= m.Duration {
			m.State = MatchStateEnding
		}
		
		// Update match logic
		m.updateGameLogic()
		
	case MatchStateEnding:
		// End match sequence
		m.End()
	}
}

func (m *Match) assignPlayerToTeam(player *Player) {
	// Balance teams - assign to team with fewer players
	team1Count := len(m.Teams[1].Players)
	team2Count := len(m.Teams[2].Players)
	
	if team1Count <= team2Count {
		player.Team = 1
		m.Teams[1].Players = append(m.Teams[1].Players, player)
	} else {
		player.Team = 2
		m.Teams[2].Players = append(m.Teams[2].Players, player)
	}
}

func (m *Match) spawnPlayer(player *Player) {
	// Set spawn position based on team/mode
	// This would use spawn points from the map
	player.State.Position.X = 0
	player.State.Position.Y = 0
	player.State.Position.Z = 0
	player.State.Health = 100
}

func (m *Match) updateGameLogic() {
	// Update match-specific game logic
	// Scoring, objectives, etc.
}

// GetMatch retrieves a match by ID
func (mm *MatchManager) GetMatch(matchID string) (*Match, bool) {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	
	match, exists := mm.matches[matchID]
	return match, exists
}

// ListMatches returns all matches
func (mm *MatchManager) ListMatches() []*Match {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	
	matches := make([]*Match, 0, len(mm.matches))
	for _, match := range mm.matches {
		matches = append(matches, match)
	}
	
	return matches
}

// FindAvailableMatch finds a match with open slots
func (mm *MatchManager) FindAvailableMatch(mode MatchMode) *Match {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	
	for _, match := range mm.matches {
		if match.Mode == mode && 
		   match.State == MatchStateWaiting && 
		   len(match.Players) < match.MaxPlayers {
			return match
		}
	}
	
	return nil
}

func generateMatchID() string {
	return fmt.Sprintf("match_%d", time.Now().UnixNano())
}