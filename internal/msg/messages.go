// Package msg contains all typed message structs used for inter-actor
// communication via NATS. Messages are plain Go structs (no protobuf).
// They are serialized into NATSEnvelope for transport.
package msg

// Direction represents a navigation direction for consolidated focus messages.
type Direction string

const (
	DirNext  Direction = "next"
	DirPrev  Direction = "prev"
	DirLeft  Direction = "left"
	DirRight Direction = "right"
	DirUp    Direction = "up"
	DirDown  Direction = "down"
)
