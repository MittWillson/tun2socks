package tunnel

import (
	"context"
	"errors"
	"fmt"
	"github.com/FlowerWrong/netstack/tcpip"
	"github.com/FlowerWrong/netstack/waiter"
	"github.com/FlowerWrong/tun2socks/configure"
	"github.com/FlowerWrong/tun2socks/dns"
	"github.com/FlowerWrong/tun2socks/util"
	"log"
	"net"
	"sync"
)

// Tcp tunnel
type TcpTunnel struct {
	wq            *waiter.Queue
	ep            tcpip.Endpoint
	socks5Conn    net.Conn
	remotePackets chan []byte // write to local
	localPackets  chan []byte // write to remote, socks5
	ctx           context.Context
	ctxCancel     context.CancelFunc
	closeOne      sync.Once    // to avoid multi close tunnel
	status        TunnelStatus // to avoid panic: send on closed channel
	rwMutex       sync.RWMutex
}

// Create a tcp tunnel
func NewTCP2Socks(wq *waiter.Queue, ep tcpip.Endpoint, ip net.IP, port uint16, fakeDns *dns.Dns, proxies *configure.Proxies) (*TcpTunnel, error) {
	socks5Conn, err := NewSocks5Conneciton(ip, port, fakeDns, proxies)
	if err != nil {
		log.Println("New socks5 conn failed", err)
		return nil, err
	}

	return &TcpTunnel{
		wq:            wq,
		ep:            ep,
		socks5Conn:    *socks5Conn,
		remotePackets: make(chan []byte, PktChannelSize),
		localPackets:  make(chan []byte, PktChannelSize),
		rwMutex:       sync.RWMutex{},
	}, nil
}

// New socks5 connection
func NewSocks5Conneciton(ip net.IP, port uint16, fakeDns *dns.Dns, proxies *configure.Proxies) (*net.Conn, error) {
	var remoteAddr string
	proxy := ""

	if fakeDns != nil {
		record := fakeDns.DnsTablePtr.GetByIP(ip)
		if record != nil {
			remoteAddr = fmt.Sprintf("%v:%d", record.Hostname, port)
			proxy = record.Proxy
		} else {
			remoteAddr = fmt.Sprintf("%v:%d", ip, port)
		}
	} else {
		remoteAddr = fmt.Sprintf("%v:%d", ip, port)
	}

	socks5Conn, err := proxies.Dial(proxy, remoteAddr)
	if err != nil {
		socks5Conn.Close()
		log.Printf("[tcp] dial %s by proxy %q failed: %s", remoteAddr, proxy, err)
		return nil, err
	}
	return &socks5Conn, nil
}

// Set tcp tunnel status with rwMutex
func (tcpTunnel *TcpTunnel) SetStatus(s TunnelStatus) {
	tcpTunnel.rwMutex.Lock()
	tcpTunnel.status = s
	tcpTunnel.rwMutex.Unlock()
}

// Get tcp tunnel status with rwMutex
func (tcpTunnel *TcpTunnel) Status() TunnelStatus {
	tcpTunnel.rwMutex.Lock()
	s := tcpTunnel.status
	tcpTunnel.rwMutex.Unlock()
	return s
}

// Start tcp tunnel
func (tcpTunnel *TcpTunnel) Run() {
	tcpTunnel.ctx, tcpTunnel.ctxCancel = context.WithCancel(context.Background())
	go tcpTunnel.writeToLocal()
	go tcpTunnel.readFromRemote()
	go tcpTunnel.writeToRemote()
	go tcpTunnel.readFromLocal()
	tcpTunnel.SetStatus(StatusProxying)
}

// Read tcp packet form local netstack
func (tcpTunnel *TcpTunnel) readFromLocal() {
	waitEntry, notifyCh := waiter.NewChannelEntry(nil)
	tcpTunnel.wq.EventRegister(&waitEntry, waiter.EventIn)
	defer tcpTunnel.wq.EventUnregister(&waitEntry)

readFromLocal:
	for {
		v, err := tcpTunnel.ep.Read(nil)
		if err != nil {
			if err == tcpip.ErrWouldBlock {
				select {
				case <-tcpTunnel.ctx.Done():
					break readFromLocal
				case <-notifyCh:
					continue readFromLocal
				}
			}
			if !util.IsClosed(err) {
				log.Println("Read from local failed", err)
			}
			tcpTunnel.Close(errors.New("read from local failed" + err.String()))
			break readFromLocal
		}
		if tcpTunnel.status != StatusClosed {
			tcpTunnel.localPackets <- v
		} else {
			break readFromLocal
		}
	}
}

// Write tcp packet to upstream
func (tcpTunnel *TcpTunnel) writeToRemote() {
writeToRemote:
	for {
		select {
		case <-tcpTunnel.ctx.Done():
			break writeToRemote
		case chunk := <-tcpTunnel.localPackets:
			// tcpTunnel.socks5Conn.SetWriteDeadline(DefaultReadWriteTimeout)
			_, err := tcpTunnel.socks5Conn.Write(chunk)
			if err != nil && !util.IsEOF(err) {
				log.Println("Write packet to remote failed", err)
				tcpTunnel.Close(err)
				break writeToRemote
			}
		}
	}
}

// Read tcp packet from upstream
func (tcpTunnel *TcpTunnel) readFromRemote() {
readFromRemote:
	for {
		select {
		case <-tcpTunnel.ctx.Done():
			break readFromRemote
		default:
			buf := make([]byte, 1500)
			// tcpTunnel.socks5Conn.SetReadDeadline(DefaultReadWriteTimeout)
			n, err := tcpTunnel.socks5Conn.Read(buf)
			if err != nil && !util.IsEOF(err) {
				log.Println("Read from remote failed", err)
				tcpTunnel.Close(err)
				break readFromRemote
			}

			if n > 0 && tcpTunnel.status != StatusClosed {
				tcpTunnel.remotePackets <- buf[0:n]
			} else {
				break readFromRemote
			}
		}
	}
}

// Write tcp packet to local netstack
func (tcpTunnel *TcpTunnel) writeToLocal() {
writeToLocal:
	for {
		select {
		case <-tcpTunnel.ctx.Done():
			break writeToLocal
		case chunk := <-tcpTunnel.remotePackets:
			_, err := tcpTunnel.ep.Write(chunk, nil)
			if err != nil {
				if !util.IsClosed(err) {
					log.Println("Write to local failed", err)
				}
				tcpTunnel.Close(errors.New(err.String()))
				break writeToLocal
			}
		}
	}
}

// Close this tcp tunnel
func (tcpTunnel *TcpTunnel) Close(reason error) {
	tcpTunnel.closeOne.Do(func() {
		tcpTunnel.SetStatus(StatusClosed)
		tcpTunnel.ctxCancel()
		tcpTunnel.socks5Conn.Close()
		tcpTunnel.ep.Close()
		close(tcpTunnel.localPackets)
		close(tcpTunnel.remotePackets)
	})
}
