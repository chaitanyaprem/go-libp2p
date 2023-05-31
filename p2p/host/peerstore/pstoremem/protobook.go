package pstoremem

import (
	"context"
	"errors"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
	pstore "github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
)

type protoSegment struct {
	sync.RWMutex
	protocols map[peer.ID]map[protocol.ID]struct{}
}

type protoSegments [256]*protoSegment

func (s *protoSegments) get(p peer.ID) *protoSegment {
	return s[byte(p[len(p)-1])]
}

type peersPerProtocol struct {
	sync.RWMutex
	peers map[protocol.ID]map[peer.ID]peer.ID
}

var errTooManyProtocols = errors.New("too many protocols")
var errNoPeersForProtocol = errors.New("no peers available for queried protocol")

type memoryProtoBook struct {
	segments protoSegments

	maxProtos int

	lk       sync.RWMutex
	interned map[protocol.ID]protocol.ID

	peers peersPerProtocol
}

var _ pstore.ProtoBook = (*memoryProtoBook)(nil)

type ProtoBookOption func(book *memoryProtoBook) error

func WithMaxProtocols(num int) ProtoBookOption {
	return func(pb *memoryProtoBook) error {
		pb.maxProtos = num
		return nil
	}
}

func NewProtoBook(opts ...ProtoBookOption) (*memoryProtoBook, error) {
	pb := &memoryProtoBook{
		interned: make(map[protocol.ID]protocol.ID, 256),
		segments: func() (ret protoSegments) {
			for i := range ret {
				ret[i] = &protoSegment{
					protocols: make(map[peer.ID]map[protocol.ID]struct{}),
				}
			}
			return ret
		}(),
		maxProtos: 1024,
	}

	for _, opt := range opts {
		if err := opt(pb); err != nil {
			return nil, err
		}
	}
	pb.peers.peers = make(map[protocol.ID]map[peer.ID]peer.ID, pb.maxProtos)
	return pb, nil
}

func (pb *memoryProtoBook) internProtocol(proto protocol.ID) protocol.ID {
	// check if it is interned with the read lock
	pb.lk.RLock()
	interned, ok := pb.interned[proto]
	pb.lk.RUnlock()

	if ok {
		return interned
	}

	// intern with the write lock
	pb.lk.Lock()
	defer pb.lk.Unlock()

	// check again in case it got interned in between locks
	interned, ok = pb.interned[proto]
	if ok {
		return interned
	}

	pb.interned[proto] = proto
	return proto
}

func (pb *memoryProtoBook) SetProtocols(p peer.ID, protos ...protocol.ID) error {
	if len(protos) > pb.maxProtos {
		return errTooManyProtocols
	}

	newprotos := make(map[protocol.ID]struct{}, len(protos))
	for _, proto := range protos {
		newprotos[pb.internProtocol(proto)] = struct{}{}
	}
	s := pb.segments.get(p)
	s.Lock()
	s.protocols[p] = newprotos
	s.Unlock()
	pb.addPeersPerProtocol(p, protos...)
	return nil
}

func (pb *memoryProtoBook) addPeersPerProtocol(p peer.ID, protos ...protocol.ID) {
	pb.peers.Lock()
	defer pb.peers.Unlock()
	for _, proto := range protos {
		peers, ok := pb.peers.peers[proto]
		if !ok {
			peers = make(map[peer.ID]peer.ID)
			peers[p] = p
			pb.peers.peers[proto] = peers
		} else {
			_, ok := peers[p]
			if !ok {
				peers[p] = p
			}
		}
	}
}

func (pb *memoryProtoBook) addProtocolsToSegment(p peer.ID, protos ...protocol.ID) error {
	s := pb.segments.get(p)

	s.Lock()
	defer s.Unlock()

	protomap, ok := s.protocols[p]
	if !ok {
		protomap = make(map[protocol.ID]struct{})
		s.protocols[p] = protomap
	}
	if len(protomap)+len(protos) > pb.maxProtos {
		return errTooManyProtocols
	}

	for _, proto := range protos {
		protomap[pb.internProtocol(proto)] = struct{}{}
	}
	return nil
}

func (pb *memoryProtoBook) AddProtocols(p peer.ID, protos ...protocol.ID) error {
	err := pb.addProtocolsToSegment(p, protos...)
	if err != nil {
		return err
	}
	pb.addPeersPerProtocol(p, protos...)
	return nil
}

func (pb *memoryProtoBook) GetProtocols(p peer.ID) ([]protocol.ID, error) {
	s := pb.segments.get(p)
	s.RLock()
	defer s.RUnlock()

	out := make([]protocol.ID, 0, len(s.protocols[p]))
	for k := range s.protocols[p] {
		out = append(out, k)
	}

	return out, nil
}

func (pb *memoryProtoBook) removeProtocolsFromSegment(p peer.ID, protos ...protocol.ID) error {
	s := pb.segments.get(p)
	s.Lock()
	defer s.Unlock()

	protomap, ok := s.protocols[p]
	if !ok {
		// nothing to remove.
		return nil
	}

	for _, proto := range protos {
		delete(protomap, pb.internProtocol(proto))
	}
	return nil
}

func (pb *memoryProtoBook) removePeersFromProtocols(p peer.ID, protos ...protocol.ID) {
	pb.peers.Lock()
	defer pb.peers.Unlock()
	for _, proto := range protos {
		if peerMap, ok := pb.peers.peers[proto]; ok {
			delete(peerMap, p)
		}
	}
}

func (pb *memoryProtoBook) RemoveProtocols(p peer.ID, protos ...protocol.ID) error {
	err := pb.removeProtocolsFromSegment(p, protos...)
	if err != nil {
		return err
	}
	pb.removePeersFromProtocols(p, protos...)
	return nil
}

func (pb *memoryProtoBook) SupportsProtocols(p peer.ID, protos ...protocol.ID) ([]protocol.ID, error) {
	s := pb.segments.get(p)
	s.RLock()
	defer s.RUnlock()

	out := make([]protocol.ID, 0, len(protos))
	for _, proto := range protos {
		if _, ok := s.protocols[p][proto]; ok {
			out = append(out, proto)
		}
	}

	return out, nil
}

func (pb *memoryProtoBook) FirstSupportedProtocol(p peer.ID, protos ...protocol.ID) (protocol.ID, error) {
	s := pb.segments.get(p)
	s.RLock()
	defer s.RUnlock()

	for _, proto := range protos {
		if _, ok := s.protocols[p][proto]; ok {
			return proto, nil
		}
	}
	return "", nil
}

func (pb *memoryProtoBook) RemovePeer(p peer.ID) {
	s := pb.segments.get(p)
	//TODO: Is a read lock required for the segment??
	pb.peers.Lock()
	for _, protos := range s.protocols {
		for proto := range protos {
			if peers, ok := pb.peers.peers[proto]; ok {
				delete(peers, p)
			}
		}
	}
	pb.peers.Unlock()

	s.Lock()
	delete(s.protocols, p)
	s.Unlock()

}

func (pb *memoryProtoBook) GetPeersForProtocol(ctx context.Context, proto protocol.ID) ([]peer.ID, error) {
	pb.peers.RLock()
	defer pb.peers.RUnlock()

	peers, ok := pb.peers.peers[proto]
	if !ok {
		return nil, errNoPeersForProtocol
	}
	peerIDs := make([]peer.ID, len(peers))
	i := 0
	for k := range peers {
		i++
		peerIDs[i] = k
	}
	return peerIDs, nil
}
