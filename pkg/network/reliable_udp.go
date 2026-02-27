package network

import (
	"sync"
	"time"
	
	pb "game-server/proto"
)

// ReliableUDP implements reliability layer over UDP
type ReliableUDP struct {
	conn           *UDPServer
	pendingAcks    map[uint64]*PendingPacket
	receivedSeqs   map[string]map[uint64]bool // playerID -> sequence -> received
	mu             sync.RWMutex
	retransmitTime time.Duration
	maxRetries     int
}

type PendingPacket struct {
	Sequence      uint64
	Data          []byte
	Addr          *net.UDPAddr
	SentTime      time.Time
	Retries       int
	AckReceived   bool
}

func NewReliableUDP(udpServer *UDPServer) *ReliableUDP {
	r := &ReliableUDP{
		conn:           udpServer,
		pendingAcks:    make(map[uint64]*PendingPacket),
		receivedSeqs:   make(map[string]map[uint64]bool),
		retransmitTime: 100 * time.Millisecond,
		maxRetries:     5,
	}
	
	go r.retransmitLoop()
	
	return r
}

// SendReliable sends a packet with reliability guarantees
func (r *ReliableUDP) SendReliable(addr *net.UDPAddr, packet *pb.GamePacket, sequence uint64) error {
	data, err := proto.Marshal(packet)
	if err != nil {
		return err
	}
	
	r.mu.Lock()
	r.pendingAcks[sequence] = &PendingPacket{
		Sequence: sequence,
		Data:     data,
		Addr:     addr,
		SentTime: time.Now(),
		Retries:  0,
	}
	r.mu.Unlock()
	
	return r.conn.SendPacket(addr, data)
}

// HandleAck processes acknowledgment packets
func (r *ReliableUDP) HandleAck(sequence uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	if pending, exists := r.pendingAcks[sequence]; exists {
		pending.AckReceived = true
		delete(r.pendingAcks, sequence)
	}
}

// MarkReceived marks a sequence as received (for deduplication)
func (r *ReliableUDP) MarkReceived(playerID string, sequence uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	if r.receivedSeqs[playerID] == nil {
		r.receivedSeqs[playerID] = make(map[uint64]bool)
	}
	
	if r.receivedSeqs[playerID][sequence] {
		return false // Duplicate
	}
	
	r.receivedSeqs[playerID][sequence] = true
	
	// Cleanup old sequences
	if len(r.receivedSeqs[playerID]) > 1000 {
		// Keep only recent 500
		newMap := make(map[uint64]bool)
		maxSeq := sequence
		for i := maxSeq - 500; i <= maxSeq; i++ {
			if r.receivedSeqs[playerID][i] {
				newMap[i] = true
			}
		}
		r.receivedSeqs[playerID] = newMap
	}
	
	return true // New packet
}

func (r *ReliableUDP) retransmitLoop() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	
	for range ticker.C {
		r.mu.Lock()
		
		now := time.Now()
		for seq, pending := range r.pendingAcks {
			if pending.AckReceived {
				delete(r.pendingAcks, seq)
				continue
			}
			
			if now.Sub(pending.SentTime) > r.retransmitTime {
				if pending.Retries >= r.maxRetries {
					// Give up
					delete(r.pendingAcks, seq)
					continue
				}
				
				// Retransmit
				r.conn.SendPacket(pending.Addr, pending.Data)
				pending.SentTime = now
				pending.Retries++
			}
		}
		
		r.mu.Unlock()
	}
}