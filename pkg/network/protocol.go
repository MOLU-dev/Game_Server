package network

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	
	pb "game-server/proto"
	"google.golang.org/protobuf/proto"
)

const (
	MaxPacketSize     = 1024 * 1024 // 1MB
	HeaderSize        = 4           // 4 bytes for length prefix
	UDPMaxPayloadSize = 1400        // MTU safe size
)

type PacketType byte

const (
	PacketTypeAuth PacketType = iota
	PacketTypeHandshake
	PacketTypeGame
	PacketTypePing
	PacketTypePong
	PacketTypeReliable
)

// ProtocolHandler handles encoding/decoding of network packets
type ProtocolHandler struct{}

func NewProtocolHandler() *ProtocolHandler {
	return &ProtocolHandler{}
}

// EncodeTCPPacket encodes a protobuf message for TCP with length prefix
func (ph *ProtocolHandler) EncodeTCPPacket(msg proto.Message) ([]byte, error) {
	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}
	
	if len(data) > MaxPacketSize {
		return nil, fmt.Errorf("packet too large: %d bytes", len(data))
	}
	
	// Create buffer with length prefix
	packet := make([]byte, HeaderSize+len(data))
	binary.BigEndian.PutUint32(packet[0:4], uint32(len(data)))
	copy(packet[4:], data)
	
	return packet, nil
}

// DecodeTCPPacket reads and decodes a TCP packet
func (ph *ProtocolHandler) DecodeTCPPacket(conn net.Conn, msg proto.Message) error {
	// Read length prefix
	lengthBuf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(conn, lengthBuf); err != nil {
		return fmt.Errorf("read length error: %w", err)
	}
	
	msgLength := binary.BigEndian.Uint32(lengthBuf)
	
	if msgLength > MaxPacketSize {
		return fmt.Errorf("packet too large: %d bytes", msgLength)
	}
	
	// Read message data
	msgBuf := make([]byte, msgLength)
	if _, err := io.ReadFull(conn, msgBuf); err != nil {
		return fmt.Errorf("read message error: %w", err)
	}
	
	// Unmarshal
	if err := proto.Unmarshal(msgBuf, msg); err != nil {
		return fmt.Errorf("unmarshal error: %w", err)
	}
	
	return nil
}

// EncodeUDPPacket encodes a protobuf message for UDP
func (ph *ProtocolHandler) EncodeUDPPacket(msg proto.Message) ([]byte, error) {
	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}
	
	if len(data) > UDPMaxPayloadSize {
		return nil, fmt.Errorf("UDP packet too large: %d bytes", len(data))
	}
	
	return data, nil
}

// DecodeUDPPacket decodes a UDP packet
func (ph *ProtocolHandler) DecodeUDPPacket(data []byte, msg proto.Message) error {
	if err := proto.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("unmarshal error: %w", err)
	}
	
	return nil
}

// SendTCPMessage sends a message over TCP
func (ph *ProtocolHandler) SendTCPMessage(conn net.Conn, msg proto.Message) error {
	packet, err := ph.EncodeTCPPacket(msg)
	if err != nil {
		return err
	}
	
	_, err = conn.Write(packet)
	return err
}

// SendUDPMessage sends a message over UDP
func (ph *ProtocolHandler) SendUDPMessage(conn *net.UDPConn, addr *net.UDPAddr, msg proto.Message) error {
	packet, err := ph.EncodeUDPPacket(msg)
	if err != nil {
		return err
	}
	
	_, err = conn.WriteToUDP(packet, addr)
	return err
}