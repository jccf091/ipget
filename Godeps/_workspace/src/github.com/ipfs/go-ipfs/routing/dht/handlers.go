package dht

import (
	"errors"
	"fmt"
	"time"

	proto "github.com/noffle/ipget/Godeps/_workspace/src/github.com/gogo/protobuf/proto"
	key "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/blocks/key"
	peer "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/p2p/peer"
	pb "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/routing/dht/pb"
	u "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/util"
	lgbl "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/util/eventlog/loggables"
	ds "github.com/noffle/ipget/Godeps/_workspace/src/github.com/jbenet/go-datastore"
	context "github.com/noffle/ipget/Godeps/_workspace/src/golang.org/x/net/context"
)

// The number of closer peers to send on requests.
var CloserPeerCount = KValue

// dhthandler specifies the signature of functions that handle DHT messages.
type dhtHandler func(context.Context, peer.ID, *pb.Message) (*pb.Message, error)

func (dht *IpfsDHT) handlerForMsgType(t pb.Message_MessageType) dhtHandler {
	switch t {
	case pb.Message_GET_VALUE:
		return dht.handleGetValue
	case pb.Message_PUT_VALUE:
		return dht.handlePutValue
	case pb.Message_FIND_NODE:
		return dht.handleFindPeer
	case pb.Message_ADD_PROVIDER:
		return dht.handleAddProvider
	case pb.Message_GET_PROVIDERS:
		return dht.handleGetProviders
	case pb.Message_PING:
		return dht.handlePing
	default:
		return nil
	}
}

func (dht *IpfsDHT) handleGetValue(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	defer log.EventBegin(ctx, "handleGetValue", p).Done()
	log.Debugf("%s handleGetValue for key: %s", dht.self, pmes.GetKey())

	// setup response
	resp := pb.NewMessage(pmes.GetType(), pmes.GetKey(), pmes.GetClusterLevel())

	// first, is there even a key?
	k := key.Key(pmes.GetKey())
	if k == "" {
		return nil, errors.New("handleGetValue but no key was provided")
		// TODO: send back an error response? could be bad, but the other node's hanging.
	}

	rec, err := dht.checkLocalDatastore(k)
	if err != nil {
		return nil, err
	}
	resp.Record = rec

	// Find closest peer on given cluster to desired key and reply with that info
	closer := dht.betterPeersToQuery(pmes, p, CloserPeerCount)
	if len(closer) > 0 {
		closerinfos := peer.PeerInfos(dht.peerstore, closer)
		for _, pi := range closerinfos {
			log.Debugf("handleGetValue returning closer peer: '%s'", pi.ID)
			if len(pi.Addrs) < 1 {
				log.Errorf(`no addresses on peer being sent!
					[local:%s]
					[sending:%s]
					[remote:%s]`, dht.self, pi.ID, p)
			}
		}

		resp.CloserPeers = pb.PeerInfosToPBPeers(dht.host.Network(), closerinfos)
	}

	return resp, nil
}

func (dht *IpfsDHT) checkLocalDatastore(k key.Key) (*pb.Record, error) {
	log.Debugf("%s handleGetValue looking into ds", dht.self)
	dskey := k.DsKey()
	iVal, err := dht.datastore.Get(dskey)
	log.Debugf("%s handleGetValue looking into ds GOT %v", dht.self, iVal)

	if err == ds.ErrNotFound {
		return nil, nil
	}

	// if we got an unexpected error, bail.
	if err != nil {
		return nil, err
	}

	// if we have the value, send it back
	log.Debugf("%s handleGetValue success!", dht.self)

	byts, ok := iVal.([]byte)
	if !ok {
		return nil, fmt.Errorf("datastore had non byte-slice value for %v", dskey)
	}

	rec := new(pb.Record)
	err = proto.Unmarshal(byts, rec)
	if err != nil {
		log.Debug("Failed to unmarshal dht record from datastore")
		return nil, err
	}

	// if its our record, dont bother checking the times on it
	if peer.ID(rec.GetAuthor()) == dht.self {
		return rec, nil
	}

	var recordIsBad bool
	recvtime, err := u.ParseRFC3339(rec.GetTimeReceived())
	if err != nil {
		log.Info("either no receive time set on record, or it was invalid: ", err)
		recordIsBad = true
	}

	if time.Now().Sub(recvtime) > MaxRecordAge {
		log.Debug("old record found, tossing.")
		recordIsBad = true
	}

	// NOTE: we do not verify the record here beyond checking these timestamps.
	// we put the burden of checking the records on the requester as checking a record
	// may be computationally expensive

	if recordIsBad {
		err := dht.datastore.Delete(dskey)
		if err != nil {
			log.Error("Failed to delete bad record from datastore: ", err)
		}

		return nil, nil // can treat this as not having the record at all
	}

	return rec, nil
}

// Store a value in this peer local storage
func (dht *IpfsDHT) handlePutValue(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	defer log.EventBegin(ctx, "handlePutValue", p).Done()
	dskey := key.Key(pmes.GetKey()).DsKey()

	if err := dht.verifyRecordLocally(pmes.GetRecord()); err != nil {
		log.Warningf("Bad dht record in PUT from: %s. %s", key.Key(pmes.GetRecord().GetAuthor()), err)
		return nil, err
	}

	rec := pmes.GetRecord()

	// record the time we receive every record
	rec.TimeReceived = proto.String(u.FormatRFC3339(time.Now()))

	data, err := proto.Marshal(rec)
	if err != nil {
		return nil, err
	}

	err = dht.datastore.Put(dskey, data)
	log.Debugf("%s handlePutValue %v", dht.self, dskey)
	return pmes, err
}

func (dht *IpfsDHT) handlePing(_ context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	log.Debugf("%s Responding to ping from %s!\n", dht.self, p)
	return pmes, nil
}

func (dht *IpfsDHT) handleFindPeer(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	defer log.EventBegin(ctx, "handleFindPeer", p).Done()
	resp := pb.NewMessage(pmes.GetType(), "", pmes.GetClusterLevel())
	var closest []peer.ID

	// if looking for self... special case where we send it on CloserPeers.
	if peer.ID(pmes.GetKey()) == dht.self {
		closest = []peer.ID{dht.self}
	} else {
		closest = dht.betterPeersToQuery(pmes, p, CloserPeerCount)
	}

	if closest == nil {
		log.Infof("%s handleFindPeer %s: could not find anything.", dht.self, p)
		return resp, nil
	}

	var withAddresses []peer.PeerInfo
	closestinfos := peer.PeerInfos(dht.peerstore, closest)
	for _, pi := range closestinfos {
		if len(pi.Addrs) > 0 {
			withAddresses = append(withAddresses, pi)
			log.Debugf("handleFindPeer: sending back '%s'", pi.ID)
		}
	}

	resp.CloserPeers = pb.PeerInfosToPBPeers(dht.host.Network(), withAddresses)
	return resp, nil
}

func (dht *IpfsDHT) handleGetProviders(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	lm := make(lgbl.DeferredMap)
	lm["peer"] = func() interface{} { return p.Pretty() }
	defer log.EventBegin(ctx, "handleGetProviders", lm).Done()

	resp := pb.NewMessage(pmes.GetType(), pmes.GetKey(), pmes.GetClusterLevel())
	key := key.Key(pmes.GetKey())
	lm["key"] = func() interface{} { return key.Pretty() }

	// debug logging niceness.
	reqDesc := fmt.Sprintf("%s handleGetProviders(%s, %s): ", dht.self, p, key)
	log.Debugf("%s begin", reqDesc)
	defer log.Debugf("%s end", reqDesc)

	// check if we have this value, to add ourselves as provider.
	has, err := dht.datastore.Has(key.DsKey())
	if err != nil && err != ds.ErrNotFound {
		log.Debugf("unexpected datastore error: %v\n", err)
		has = false
	}

	// setup providers
	providers := dht.providers.GetProviders(ctx, key)
	if has {
		providers = append(providers, dht.self)
		log.Debugf("%s have the value. added self as provider", reqDesc)
	}

	if providers != nil && len(providers) > 0 {
		infos := peer.PeerInfos(dht.peerstore, providers)
		resp.ProviderPeers = pb.PeerInfosToPBPeers(dht.host.Network(), infos)
		log.Debugf("%s have %d providers: %s", reqDesc, len(providers), infos)
	}

	// Also send closer peers.
	closer := dht.betterPeersToQuery(pmes, p, CloserPeerCount)
	if closer != nil {
		infos := peer.PeerInfos(dht.peerstore, closer)
		resp.CloserPeers = pb.PeerInfosToPBPeers(dht.host.Network(), infos)
		log.Debugf("%s have %d closer peers: %s", reqDesc, len(closer), infos)
	}

	return resp, nil
}

func (dht *IpfsDHT) handleAddProvider(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	lm := make(lgbl.DeferredMap)
	lm["peer"] = func() interface{} { return p.Pretty() }

	defer log.EventBegin(ctx, "handleAddProvider", lm).Done()
	key := key.Key(pmes.GetKey())
	lm["key"] = func() interface{} { return key.Pretty() }

	log.Debugf("%s adding %s as a provider for '%s'\n", dht.self, p, key)

	// add provider should use the address given in the message
	pinfos := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
	for _, pi := range pinfos {
		if pi.ID != p {
			// we should ignore this provider reccord! not from originator.
			// (we chould sign them and check signature later...)
			log.Debugf("handleAddProvider received provider %s from %s. Ignore.", pi.ID, p)
			continue
		}

		if len(pi.Addrs) < 1 {
			log.Debugf("%s got no valid addresses for provider %s. Ignore.", dht.self, p)
			continue
		}

		log.Infof("received provider %s for %s (addrs: %s)", p, key, pi.Addrs)
		if pi.ID != dht.self { // dont add own addrs.
			// add the received addresses to our peerstore.
			dht.peerstore.AddAddrs(pi.ID, pi.Addrs, peer.ProviderAddrTTL)
		}
		dht.providers.AddProvider(ctx, key, p)
	}

	return nil, nil
}
