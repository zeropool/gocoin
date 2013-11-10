package network

import (
	"time"
	"bytes"
	"errors"
	"encoding/binary"
	"github.com/piotrnar/gocoin/btc"
	"github.com/piotrnar/gocoin/client/common"
)


func (c *OneConnection) SendVersion() {
	b := bytes.NewBuffer([]byte{})

	binary.Write(b, binary.LittleEndian, uint32(common.Version))
	binary.Write(b, binary.LittleEndian, uint64(common.Services))
	binary.Write(b, binary.LittleEndian, uint64(time.Now().Unix()))

	b.Write(c.PeerAddr.NetAddr.Bytes())
	if ExternalAddrLen()>0 {
		b.Write(BestExternalAddr())
	} else {
		b.Write(bytes.Repeat([]byte{0}, 26))
	}

	b.Write(nonce[:])

	b.WriteByte(byte(len(common.CFG.UserAgent)))
	b.Write([]byte(common.CFG.UserAgent))

	common.Last.Mutex.Lock()
	binary.Write(b, binary.LittleEndian, uint32(common.Last.Block.Height))
	common.Last.Mutex.Unlock()
	if !common.CFG.TXPool.Enabled {
		b.WriteByte(0)  // don't notify me about txs
	}

	c.SendRawMsg("version", b.Bytes())
}



func (c *OneConnection) HandleVersion(pl []byte) error {
	if len(pl) >= 80 /*Up to, includiong, the nonce */ {
		c.Mutex.Lock()
		c.Node.Version = binary.LittleEndian.Uint32(pl[0:4])
		if bytes.Equal(pl[72:80], nonce[:]) {
			c.Mutex.Unlock()
			return errors.New("Connecting to ourselves")
		}
		if c.Node.Version < MIN_PROTO_VERSION {
			c.Mutex.Unlock()
			return errors.New("Client version too low")
		}
		c.Node.Services = binary.LittleEndian.Uint64(pl[4:12])
		c.Node.Timestamp = binary.LittleEndian.Uint64(pl[12:20])
		c.Mutex.Unlock()
		if ValidIp4(pl[40:44]) {
			ExternalIpMutex.Lock()
			ExternalIp4[binary.BigEndian.Uint32(pl[40:44])]++
			ExternalIpMutex.Unlock()
		}
		if len(pl) >= 86 {
			c.Mutex.Lock()
			le, of := btc.VLen(pl[80:])
			of += 80
			c.Node.Agent = string(pl[of:of+le])
			of += le
			if len(pl) >= of+4 {
				c.Node.Height = binary.LittleEndian.Uint32(pl[of:of+4])
			}
			c.Mutex.Unlock()
		}
	} else {
		return errors.New("common.Version message too short")
	}
	c.SendRawMsg("verack", []byte{})
	return nil
}