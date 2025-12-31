package steam

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"golang.org/x/net/proxy"
	"hash/crc32"
	"io/ioutil"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adiecho/go-steam/v3/cryptoutil"
	"github.com/adiecho/go-steam/v3/netutil"
	"github.com/adiecho/go-steam/v3/protocol"
	"github.com/adiecho/go-steam/v3/protocol/protobuf"
	"github.com/adiecho/go-steam/v3/protocol/steamlang"
	"github.com/adiecho/go-steam/v3/steamid"
)

// Represents a client to the Steam network.
// Always poll events from the channel returned by Events() or receiving messages will stop.
// All access, unless otherwise noted, should be threadsafe.
//
// When a FatalErrorEvent is emitted, the connection is automatically closed. The same client can be used to reconnect.
// Other errors don't have any effect.
type Client struct {
	// these need to be 64 bit aligned for sync/atomic on 32bit
	sessionId    int32
	_            uint32
	steamId      uint64
	currentJobId uint64

	Auth          *Auth
	Social        *Social
	Web           *Web
	Notifications *Notifications
	Trading       *Trading
	GC            *GameCoordinator

	events        chan interface{}
	handlers      []PacketHandler
	handlersMutex sync.RWMutex

	jobHandlersMutex sync.RWMutex
	jobHandlers      map[protocol.JobId]chan *protocol.Packet
	tempSessionKey   []byte

	ConnectionTimeout time.Duration

	mutex     sync.RWMutex // guarding conn and writeChan
	conn      connection
	writeChan chan protocol.IMsg
	writeBuf  *bytes.Buffer
	heartbeat *time.Ticker

	proxyDialer *proxy.Dialer
}

type PacketHandler interface {
	HandlePacket(*protocol.Packet)
}

func NewClient() *Client {
	client := &Client{
		events:   make(chan interface{}, 3),
		writeBuf: new(bytes.Buffer),
	}

	client.Auth = &Auth{client: client}
	client.RegisterPacketHandler(client.Auth)

	client.Social = newSocial(client)
	client.RegisterPacketHandler(client.Social)

	client.Web = &Web{client: client}
	client.RegisterPacketHandler(client.Web)

	client.Notifications = newNotifications(client)
	client.RegisterPacketHandler(client.Notifications)

	client.Trading = &Trading{client: client}
	client.RegisterPacketHandler(client.Trading)

	client.GC = newGC(client)
	client.RegisterPacketHandler(client.GC)

	client.jobHandlers = map[protocol.JobId]chan *protocol.Packet{}

	return client
}

// Get the event channel. By convention all events are pointers, except for errors.
// It is never closed.
func (c *Client) Events() <-chan interface{} {
	return c.events
}

func (c *Client) Emit(event interface{}) {
	c.events <- event
}

// Emits a FatalErrorEvent formatted with fmt.Errorf and disconnects.
func (c *Client) Fatalf(format string, a ...interface{}) {
	c.Emit(FatalErrorEvent(fmt.Errorf(format, a...)))
	c.Disconnect()
}

// Emits an error formatted with fmt.Errorf.
func (c *Client) Errorf(format string, a ...interface{}) {
	c.Emit(fmt.Errorf(format, a...))
}

// Registers a PacketHandler that receives all incoming packets.
func (c *Client) RegisterPacketHandler(handler PacketHandler) {
	c.handlersMutex.Lock()
	defer c.handlersMutex.Unlock()
	c.handlers = append(c.handlers, handler)
}

func (c *Client) WaitForJobPacket(jobID protocol.JobId) chan *protocol.Packet {
	c.jobHandlersMutex.Lock()
	defer c.jobHandlersMutex.Unlock()
	ch := make(chan *protocol.Packet, 1)
	c.jobHandlers[jobID] = ch
	return ch
}

func (c *Client) GetNextJobId() protocol.JobId {
	return protocol.JobId(atomic.AddUint64(&c.currentJobId, 1))
}

func (c *Client) SteamId() steamid.SteamId {
	return steamid.SteamId(atomic.LoadUint64(&c.steamId))
}

func (c *Client) SessionId() int32 {
	return atomic.LoadInt32(&c.sessionId)
}

func (c *Client) Connected() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.conn != nil
}

// SetProxyDialer to use for Steam connections
// Disconnects the connection if it is currently connected
func (c *Client) SetProxyDialer(dialer *proxy.Dialer) {
	if c.Connected() {
		c.Disconnect()
	}

	c.proxyDialer = dialer
}

// Connects to a random Steam server and returns its address.
// If this client is already connected, it is disconnected first.
// This method tries to use an address from the Steam Directory and falls
// back to the built-in server list if the Steam Directory can't be reached.
// If you want to connect to a specific server, use `ConnectTo`.
func (c *Client) Connect() (*netutil.PortAddr, error) {
	var server *netutil.PortAddr

	// try to initialize the directory cache
	if !steamDirectoryCache.IsInitialized() {
		_ = steamDirectoryCache.Initialize()
	}
	if steamDirectoryCache.IsInitialized() {
		server = steamDirectoryCache.GetRandomCM()
	} else {
		server = GetRandomCM()
	}

	err := c.ConnectTo(server)
	return server, err
}

// Connects to a specific server.
// You may want to use one of the `GetRandom*CM()` functions in this package.
// If this client is already connected, it is disconnected first.
func (c *Client) ConnectTo(addr *netutil.PortAddr) error {
	return c.ConnectToBind(addr, nil)
}

// Connects to a specific server, and binds to a specified local IP
// If this client is already connected, it is disconnected first.
func (c *Client) ConnectToBind(addr *netutil.PortAddr, local *net.TCPAddr) error {
	c.Disconnect()

	conn, err := dialTCP(c.proxyDialer, local, addr.ToTCPAddr())
	if err != nil {
		c.Fatalf("Connect failed: %v", err)
		return err
	}
	c.conn = conn
	c.writeChan = make(chan protocol.IMsg, 5)

	go c.readLoop()
	go c.writeLoop()

	return nil
}

func (c *Client) Disconnect() {
	if c.Connected() {
		// Gracefully logout, we know we have a connection
		c.Write(protocol.NewClientMsgProtobuf(steamlang.EMsg_ClientLogOff, new(protobuf.CMsgClientLogOff)))
		time.Sleep(time.Second * 3)
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.conn == nil {
		return
	}

	c.conn.Close()
	c.conn = nil
	if c.heartbeat != nil {
		c.heartbeat.Stop()
	}
	close(c.writeChan)
	c.Emit(&DisconnectedEvent{})

}

// Adds a message to the send queue. Modifications to the given message after
// writing are not allowed (possible race conditions).
//
// Writes to this client when not connected are ignored.
func (c *Client) Write(msg protocol.IMsg) {
	if cm, ok := msg.(protocol.IClientMsg); ok {
		cm.SetSessionId(c.SessionId())
		cm.SetSteamId(c.SteamId())
	}
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if c.conn == nil {
		return
	}
	c.writeChan <- msg
}

func (c *Client) readLoop() {
	for {
		// This *should* be atomic on most platforms, but the Go spec doesn't guarantee it
		c.mutex.RLock()
		conn := c.conn
		c.mutex.RUnlock()
		if conn == nil {
			return
		}
		packet, err := conn.Read()

		if err != nil {
			c.Fatalf("Error reading from the connection: %v", err)
			return
		}
		c.handlePacket(packet)
	}
}

func (c *Client) writeLoop() {
	for {
		c.mutex.RLock()
		conn := c.conn
		c.mutex.RUnlock()
		if conn == nil {
			return
		}

		msg, ok := <-c.writeChan
		if !ok {
			return
		}

		err := msg.Serialize(c.writeBuf)
		if err != nil {
			c.writeBuf.Reset()
			c.Fatalf("Error serializing message %v: %v", msg, err)
			return
		}

		err = conn.Write(c.writeBuf.Bytes())

		c.writeBuf.Reset()

		if err != nil {
			c.Fatalf("Error writing message %v: %v", msg, err)
			return
		}
	}
}

func (c *Client) heartbeatLoop(seconds time.Duration) {
	if c.heartbeat != nil {
		c.heartbeat.Stop()
	}
	c.heartbeat = time.NewTicker(seconds * time.Second)
	defer func() {
		c.heartbeat = nil
	}()

	for {
		if c.heartbeat == nil {
			break
		}

		select {
		case _, ok := <-c.heartbeat.C:
			if !ok {
				return
			}
			c.Write(protocol.NewClientMsgProtobuf(steamlang.EMsg_ClientHeartBeat, new(protobuf.CMsgClientHeartBeat)))
		case <-time.After(5 * time.Minute):
			// failed to get heartbeat tick after 5 minutes, returning early
			return
		}
	}
}

func (c *Client) handlePacket(packet *protocol.Packet) {
	switch packet.EMsg {
	case steamlang.EMsg_ChannelEncryptRequest:
		c.handleChannelEncryptRequest(packet)
	case steamlang.EMsg_ChannelEncryptResult:
		c.handleChannelEncryptResult(packet)
	case steamlang.EMsg_Multi:
		c.handleMulti(packet)
	}

	c.handlersMutex.RLock()
	defer c.handlersMutex.RUnlock()
	for _, handler := range c.handlers {
		handler.HandlePacket(packet)
	}

	c.jobHandlersMutex.RLock()
	defer c.jobHandlersMutex.RUnlock()
	if ch, ok := c.jobHandlers[packet.TargetJobId]; ok {
		ch <- packet
		delete(c.jobHandlers, packet.TargetJobId)
	}
}

func (c *Client) handleChannelEncryptRequest(packet *protocol.Packet) {
	body := steamlang.NewMsgChannelEncryptRequest()
	packet.ReadMsg(body)

	if body.Universe != steamlang.EUniverse_Public {
		c.Fatalf("Invalid univserse %v!", body.Universe)
	}

	c.tempSessionKey = make([]byte, 32)
	rand.Read(c.tempSessionKey)
	encryptedKey := cryptoutil.RSAEncrypt(GetPublicKey(steamlang.EUniverse_Public), c.tempSessionKey)

	payload := new(bytes.Buffer)
	payload.Write(encryptedKey)
	binary.Write(payload, binary.LittleEndian, crc32.ChecksumIEEE(encryptedKey))
	payload.WriteByte(0)
	payload.WriteByte(0)
	payload.WriteByte(0)
	payload.WriteByte(0)

	c.Write(protocol.NewMsg(steamlang.NewMsgChannelEncryptResponse(), payload.Bytes()))
}

func (c *Client) handleChannelEncryptResult(packet *protocol.Packet) {
	body := steamlang.NewMsgChannelEncryptResult()
	packet.ReadMsg(body)

	if body.Result != steamlang.EResult_OK {
		c.Fatalf("Encryption failed: %v", body.Result)
		return
	}
	c.conn.SetEncryptionKey(c.tempSessionKey)
	c.tempSessionKey = nil

	c.Emit(&ConnectedEvent{})
}

func (c *Client) handleMulti(packet *protocol.Packet) {
	body := new(protobuf.CMsgMulti)
	packet.ReadProtoMsg(body)

	payload := body.GetMessageBody()

	if body.GetSizeUnzipped() > 0 {
		r, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			c.Errorf("handleMulti: Error while decompressing: %v", err)
			return
		}

		payload, err = ioutil.ReadAll(r)
		if err != nil {
			c.Errorf("handleMulti: Error while decompressing: %v", err)
			return
		}
	}

	pr := bytes.NewReader(payload)
	for pr.Len() > 0 {
		var length uint32
		binary.Read(pr, binary.LittleEndian, &length)
		packetData := make([]byte, length)
		pr.Read(packetData)
		p, err := protocol.NewPacket(packetData)
		if err != nil {
			c.Errorf("Error reading packet in Multi msg %v: %v", packet, err)
			continue
		}
		c.handlePacket(p)
	}
}

func readIp(ip uint32) net.IP {
	r := make(net.IP, 4)
	r[3] = byte(ip)
	r[2] = byte(ip >> 8)
	r[1] = byte(ip >> 16)
	r[0] = byte(ip >> 24)
	return r
}
