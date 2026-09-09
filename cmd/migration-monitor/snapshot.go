package main

// The /api/state document the page draws. Everything derived - lineage,
// lanes, depth, agreement, expected - is computed in Go; the page only draws.
// Slots count from genesis. Roots: a block's header root is the primary (MPT
// before I*, PBT after); each holder's shadow root is the other format, and
// agreement is judged on the shadow against the majority class.

type apiState struct {
	Seq           uint64       `json:"seq"` // unchanged => nothing to redraw
	NowSlot       uint64       `json:"now_slot"`
	Truncated     int          `json:"truncated"` // head moves left unjudged because the store lost their ancestry
	SlotSeconds   uint64       `json:"slot_seconds"`
	ForkSlot      uint64       `json:"fork_slot"`
	FinalizedSlot uint64       `json:"finalized_slot"`
	Nodes         []nodeView   `json:"nodes"`
	Segments      []segment    `json:"segments"`
	Blocks        []blockView  `json:"blocks"`
	Reorgs        []reorg      `json:"reorgs"`
	Partitions    []partition  `json:"partitions"`
	Schedule      []scheduleOp `json:"schedule"`
	Alerts        []alert      `json:"alerts"`
}

type nodeView struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Head           string `json:"head"`
	HeadNumber     uint64 `json:"head_number"`
	HeadSlot       uint64 `json:"head_slot"`
	Segment        string `json:"segment"`
	Phase          string `json:"phase"` // synced|following|parked|window|done|stalled|unknown
	CursorNumber   uint64 `json:"cursor_number"`
	CursorHash     string `json:"cursor_hash"`
	Lag            uint64 `json:"lag"`
	CursorDetached bool   `json:"cursor_detached"`
	IStar          string `json:"istar"` // none|provisional|final
	ELPeers        int    `json:"el_peers"`
	CLPeers        int    `json:"cl_peers"` // -1 when no beacon endpoint is configured
	FinalizedSlot  uint64 `json:"finalized_slot"`
	Status         string `json:"status"` // ok|unreachable
	Isolated       bool   `json:"isolated"`
}

// segment is a maximal single-lineage run; lane 0 is the canonical chain.
type segment struct {
	ID        string `json:"id"`
	Lane      int    `json:"lane"`
	Ancestor  string `json:"ancestor"` // the canonical block it forked from ("" for the spine)
	FirstSlot uint64 `json:"first_slot"`
	LastSlot  uint64 `json:"last_slot"`
	Blocks    uint64 `json:"blocks"`
	Tip       string `json:"tip"`
	State     string `json:"state"`   // live|orphaned
	Holders   []int  `json:"holders"` // live: nodes whose head is on it
	LostBy    []int  `json:"lost_by"` // orphaned: nodes that were on it when it lost
}

type blockView struct {
	Hash          string            `json:"hash"`
	Parent        string            `json:"parent"`
	Number        uint64            `json:"number"`
	Slot          uint64            `json:"slot"`
	Segment       string            `json:"segment"`
	Format        string            `json:"format"` // mpt|pbt: the primary root's format
	IStar         bool              `json:"istar"`
	IStarOrphaned bool              `json:"istar_orphaned"`
	StateRoot     string            `json:"state_root"`   // header root (primary)
	ShadowRoots   map[string]string `json:"shadow_roots"` // node id -> shadow root; absent = not reported
	Agreement     string            `json:"agreement"`    // pending|partial|all|single|split|gone
	Dissent       []int             `json:"dissent"`
}

type reorg struct {
	Slot           uint64 `json:"slot"`
	Node           int    `json:"node"`
	Depth          uint64 `json:"depth"`
	Added          uint64 `json:"added"`
	Ancestor       string `json:"ancestor"`
	AncestorNumber uint64 `json:"ancestor_number"`
	OldHead        string `json:"old_head"`
	NewHead        string `json:"new_head"`
	LostSegment    string `json:"lost_segment"`
	WonSegment     string `json:"won_segment"`
	CrossesIStar   bool   `json:"crosses_istar"`
}

type partition struct {
	Name        string `json:"name"`
	Class       string `json:"class"` // deep|short|straddle|window|scenario
	Victims     []int  `json:"victims"`
	AppliedSlot uint64 `json:"applied_slot"`
	LiftedSlot  uint64 `json:"lifted_slot"` // 0 = still applied
}

type scheduleOp struct {
	Name      string `json:"name"`
	Class     string `json:"class"`
	Victims   []int  `json:"victims"`
	StartSlot uint64 `json:"start_slot"`
	EndSlot   uint64 `json:"end_slot"`
}

type alert struct {
	Slot     uint64 `json:"slot"`
	Kind     string `json:"kind"` // warn|critical
	Node     int    `json:"node"`
	Expected bool   `json:"expected"`
	Detail   string `json:"detail"`
}
