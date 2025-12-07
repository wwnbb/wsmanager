package states

type ConnectionState int32

const (
	StateNew ConnectionState = iota // connection error occurred, can be reconnected
	StateConnecting
	StateConnected
	StateDisconnected
)

var states = [...]string{
	"New",
	"Connecting",
	"Connected",
	"Disconnected",
}

func (s ConnectionState) String() string {
	return states[s]
}
