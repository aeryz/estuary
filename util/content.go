package util

import (
	"context"
	"time"

	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	"gorm.io/gorm"
)

type ContentType int64

const (
	Unknown ContentType = iota
	File
	Directory
)

type ContentInCollection struct {
	Collection     string `json:"collection"`
	CollectionPath string `json:"collectionPath"`
}

type ContentAddIpfsBody struct {
	ContentInCollection

	Root  string   `json:"root"`
	Name  string   `json:"name"`
	Peers []string `json:"peers"`
}

type ContentAddResponse struct {
	Cid       string   `json:"cid"`
	EstuaryId uint     `json:"estuaryId"`
	Providers []string `json:"providers"`
}

type ContentCreateBody struct {
	ContentInCollection

	Root     string      `json:"root"`
	Name     string      `json:"name"`
	Location string      `json:"location"`
	Type     ContentType `json:"type"`
}

type ContentCreateResponse struct {
	ID uint `json:"id"`
}

type Content struct {
	ID        uint           `gorm:"primarykey" json:"id"`
	CreatedAt time.Time      `json:"-"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`

	Cid         DbCID       `json:"cid"`
	Name        string      `json:"name"`
	UserID      uint        `json:"userId" gorm:"index"`
	Description string      `json:"description"`
	Size        int64       `json:"size"`
	Type        ContentType `json:"type"`
	Active      bool        `json:"active"`
	Offloaded   bool        `json:"offloaded"`
	Replication int         `json:"replication"`

	// TODO: shift most of the 'state' booleans in here into a single state
	// field, should make reasoning about things much simpler
	AggregatedIn uint `json:"aggregatedIn" gorm:"index:,option:CONCURRENTLY"`
	Aggregate    bool `json:"aggregate"`

	Pinning bool   `json:"pinning"`
	PinMeta string `json:"pinMeta"`

	Failed bool `json:"failed"`

	Location string `json:"location"`
	// TODO: shift location tracking to just use the ID of the shuttle
	// Also move towards recording content movement intentions in the database,
	// making that process more resilient to failures
	// LocID     uint   `json:"locID"`
	// LocIntent uint   `json:"locIntent"`

	// If set, this content is part of a split dag.
	// In such a case, the 'root' content should be advertised on the dht, but
	// not have deals made for it, and the children should have deals made for
	// them (unlike with aggregates)
	DagSplit  bool `json:"dagSplit"`
	SplitFrom uint `json:"splitFrom"`
}

type Object struct {
	ID         uint  `gorm:"primarykey"`
	Cid        DbCID `gorm:"index"`
	Size       int
	Reads      int
	LastAccess time.Time
}

type ObjRef struct {
	ID        uint `gorm:"primarykey"`
	Content   uint `gorm:"index:,option:CONCURRENTLY"`
	Object    uint `gorm:"index:,option:CONCURRENTLY"`
	Offloaded uint
}

// FindCIDType checks if a pinned CID (root) is a file, a dir or unknown
// Returns dbmgr.File or dbmgr.Directory on success
// Returns dbmgr.Unknown otherwise
func FindCIDType(ctx context.Context, root cid.Cid, dserv ipld.NodeGetter) (contentType ContentType) {
	contentType = Unknown
	nilCID := cid.Cid{}
	if root == nilCID || dserv == nil {
		return
	}

	nd, err := dserv.Get(ctx, root)
	if err != nil {
		return
	}

	contentType = File
	fsNode, err := TryExtractFSNode(nd)
	if err != nil {
		return
	}

	if fsNode.IsDir() {
		contentType = Directory
	}
	return
}
