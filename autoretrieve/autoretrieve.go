package autoretrieve

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/application-research/estuary/util"
	provider "github.com/filecoin-project/index-provider"
	"github.com/filecoin-project/index-provider/metadata"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"gorm.io/gorm"
)

type Autoretrieve struct {
	gorm.Model

	Handle            string `gorm:"unique"`
	Token             string `gorm:"unique"`
	LastConnection    time.Time
	LastAdvertisement time.Time
	PrivateKey        string `gorm:"unique"`
	Addresses         string
}

type HeartbeatAutoretrieveResponse struct {
	Handle            string         `json:"handle"`
	LastConnection    time.Time      `json:"lastConnection"`
	LastAdvertisement time.Time      `json:"lastAdvertisement"`
	AddrInfo          *peer.AddrInfo `json:"addrInfo"`
}

type AutoretrieveListResponse struct {
	Handle            string         `json:"handle"`
	LastConnection    time.Time      `json:"lastConnection"`
	LastAdvertisement time.Time      `json:"lastAdvertisement"`
	AddrInfo          *peer.AddrInfo `json:"addrInfo"`
}

type AutoretrieveInitResponse struct {
	Handle         string         `json:"handle"`
	Token          string         `json:"token"`
	LastConnection time.Time      `json:"lastConnection"`
	AddrInfo       *peer.AddrInfo `json:"addrInfo"`
}

type SimpleEstuaryMhIterator struct {
	offset int
	Mh     []multihash.Multihash
}

func (m *SimpleEstuaryMhIterator) Next() (multihash.Multihash, error) {
	if m.offset < len(m.Mh) {
		hash := m.Mh[m.offset]
		m.offset++
		return hash, nil
	}
	return nil, io.EOF
}

// newIndexProvider creates a new index-provider engine to send announcements to storetheindex
// this needs to keep running continuously because storetheindex
// will come to fetch advertisements "when it feels like it"
func NewAutoretrieveEngine(stopCh chan struct{}, tickInterval time.Duration, db *gorm.DB) (*AutoretrieveEngine, error) {
	host, err := libp2p.New()
	if err != nil {
		return nil, err
	}
	topic := "/indexer/ingest/mainnet"
	indexerMultiaddr, err := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/3003/p2p/12D3KooWChQyVH7a3iR3o8kmdYwXiHf2v3tXQWhSCS9j8NbLVQ9o") //TODO: need to adjust p2p addr
	if err != nil {
		return nil, err
	}
	indexerAddrinfo, err := peer.AddrInfosFromP2pAddrs(indexerMultiaddr)
	if err != nil {
		return nil, err
	}
	pubG, err := pubsub.NewGossipSub(context.Background(), host,
		pubsub.WithDirectConnectTicks(1),
		pubsub.WithDirectPeers(indexerAddrinfo),
	)
	if err != nil {
		return nil, err
	}
	pubT, err := pubG.Join(topic)
	if err != nil {
		return nil, err
	}

	newEngine, err := New(
		WithTopic(pubT),      // TODO: remove, testing
		WithTopicName(topic), // TODO: remove, testing
		WithHost(host),       // need to be localhost/estuary
		WithPublisherKind(DataTransferPublisher),
	)
	if err != nil {
		return nil, err
	}

	// Create index-provider engine (s.Node.IndexProvider) to send announcements to
	// this needs to keep running continuously because storetheindex
	// will come to fetch for advertisements "when it feels like it"
	newEngine.RegisterMultihashLister(func(ctx context.Context, contextID []byte) (provider.MultihashIterator, error) {

		arHandle := contextID // contextID is the autoretrieve handle
		if err != nil {
			return nil, err
		}

		var ar Autoretrieve
		// get the autoretrieve entry from the database
		err = db.Find(&ar, "handle = ?", arHandle).Error
		if err != nil {
			return nil, err
		}

		var newContents []util.Content
		// find all new multihashes since the last time we advertised for this autoretrieve server
		err = db.Find(&newContents, "active = true and created_at >= ?", ar.LastAdvertisement).Error
		if err != nil {
			return nil, err
		}

		multihashes := []multihash.Multihash{}
		for _, content := range newContents {
			multihashes = append(multihashes, content.Cid.CID.Hash())
		}

		return &SimpleEstuaryMhIterator{
			Mh: multihashes,
		}, nil
	})

	newEngine.stopCh = stopCh
	newEngine.tickInterval = tickInterval
	newEngine.db = db

	// start engine
	newEngine.Start(context.Background())

	return newEngine, nil
}

func (arEng *AutoretrieveEngine) Run() {
	var autoretrieves []Autoretrieve
	var lastTickTime time.Time
	var curTime time.Time
	var newContextID []byte

	// start ticker
	ticker := time.NewTicker(arEng.tickInterval)
	defer ticker.Stop()

	for {
		curTime = time.Now()
		lastTickTime = curTime.Add(-arEng.tickInterval)
		// Find all autoretrieve servers that are online (that sent heartbeat)
		err := arEng.db.Find(&autoretrieves, "last_connection > ?", lastTickTime).Error
		if err != nil {
			log.Errorf("unable to query autoretrieve servers from database: %s", err)
			return
		}
		if len(autoretrieves) == 0 {
			log.Infof("no autoretrieve servers online")
			// wait for next tick, or quit
			select {
			case <-ticker.C:
				continue
			case <-arEng.stopCh:
				break
			}
		}

		log.Infof("announcing new CIDs to %d autoretrieve servers", len(autoretrieves))
		// send announcement with new CIDs for each autoretrieve server
		for _, ar := range autoretrieves {

			newContextID = []byte(ar.Handle)

			retrievalAddresses := []string{}
			providerID := ""
			for _, fullAddr := range strings.Split(ar.Addresses, ",") {
				arAddrInfo, err := peer.AddrInfoFromString(fullAddr)
				if err != nil {
					log.Errorf("could not parse multiaddress '%s': %s", fullAddr, err)
					continue
				}
				providerID = arAddrInfo.ID.String()
				retrievalAddresses = append(retrievalAddresses, arAddrInfo.Addrs[0].String())
			}
			if providerID == "" {
				log.Errorf("no providerID for autoretrieve %s, skipping", ar.Handle)
				continue
			}
			if len(retrievalAddresses) == 0 {
				log.Errorf("no retrieval addresses for autoretrieve %s, skipping", ar.Handle)
				continue
			}

			var newContentsCount int64
			err = arEng.db.Model(&util.Content{}).Where("active = true and created_at >= ?", ar.LastAdvertisement).Count(&newContentsCount).Error
			if err != nil {
				log.Errorf("unable to query new CIDs from database: %s", err)
				continue
			}
			if newContentsCount == 0 {
				log.Debugf("no new CIDs to announce, skipping")
				continue
			}
			log.Debugf("found %d new CIDs, announcing", newContentsCount)

			log.Infof("sending announcement to %s", ar.Handle)
			adCid, err := arEng.NotifyPut(context.Background(), newContextID, providerID, retrievalAddresses, metadata.New(metadata.Bitswap{}))
			if err != nil {
				log.Errorf("could not announce new CIDs: %s", err)
				continue
			}

			// update lastAdvertisement time on database
			if err := arEng.db.Model(Autoretrieve{}).UpdateColumn("lastAdvertisement", time.Now()).Error; err != nil {
				log.Errorf("unable to update advertisement time on database: %s", err)
				return
			}

			log.Infof("announced new CIDs: %s", adCid)
		}

		// wait for next tick, or quit
		select {
		case <-ticker.C:
			continue
		case <-arEng.stopCh:
			break
		}
	}
}

// ValidateAddresses checks to see if all multiaddresses are valid
// returns empty []string if all multiaddresses are valid strings
// returns a list of all invalid multiaddresses if any is invalid
func validateAddresses(addresses []string) []string {
	var invalidAddresses []string
	for _, addr := range addresses {
		_, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			invalidAddresses = append(invalidAddresses, addr)
		}
	}
	return invalidAddresses
}

func ValidatePeerInfo(privKeyStr string, addresses []string) (*peer.AddrInfo, error) {
	// check if peerid is correct
	privateKey, err := stringToPrivKey(privKeyStr)
	if err != nil {
		return nil, fmt.Errorf("unable to decode private key: %s", err)
	}
	_, err = peer.IDFromPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid peer information: %s", err)
	}

	if len(addresses) == 0 || addresses[0] == "" {
		return nil, fmt.Errorf("no addresses provided")
	}

	// check if multiaddresses formats are correct
	invalidAddrs := validateAddresses(addresses)
	if len(invalidAddrs) != 0 {
		return nil, fmt.Errorf("invalid address(es): %s", strings.Join(invalidAddrs, ", "))
	}

	// any of the multiaddresses of the peer should work to get addrInfo
	// we get the first one
	addrInfo, err := peer.AddrInfoFromString(addresses[0])
	if err != nil {
		return nil, err
	}

	return addrInfo, nil
}

func stringToPrivKey(privKeyStr string) (crypto.PrivKey, error) {
	privKeyBytes, err := crypto.ConfigDecodeKey(privKeyStr)
	if err != nil {
		return nil, err
	}

	privKey, err := crypto.UnmarshalPrivateKey(privKeyBytes)
	if err != nil {
		return nil, err
	}

	return privKey, nil
}

func multiAddrsToString(addrs []multiaddr.Multiaddr) []string {
	var rAddrs []string
	for _, addr := range addrs {
		rAddrs = append(rAddrs, addr.String())
	}
	return rAddrs
}

func stringToMultiAddrs(addrStr string) ([]multiaddr.Multiaddr, error) {
	var mAddrs []multiaddr.Multiaddr
	for _, addr := range strings.Split(addrStr, ",") {
		ma, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			return nil, err
		}
		mAddrs = append(mAddrs, ma)
	}
	return mAddrs, nil
}
