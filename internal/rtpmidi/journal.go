package rtpmidi

// The sender-side recovery journal (RFC 6295 §5, Appendix A/B) is NOT
// implemented. Config.Journal is accepted and ignored: packets are sent with
// the J flag clear, which is valid (the journal is optional on the wire and
// every receiver handles its absence).
//
// Why not: a conforming journal needs the checkpoint-history bookkeeping, the
// S-bit rules, chapters P, C, M, W, N, E, T, A and the system chapters, and
// subtle state (toggle/count tools, parameter-system transactions, Reset State
// handling). A partial journal could not be checked against any real receiver
// here, and one that is subtly wrong is worse than none. What EMM does instead
// is release the notes a peer holds when packets from it are lost or when it
// leaves (Config.ReleaseNotesOnLoss), which removes stuck notes, the one
// indefinite artifact that matters most in practice.
//
// The receive side never reads a peer's journal; it always follows the command
// list, so journals sent by Apple's stack are skipped by construction.

// journalState is the placeholder for per-peer journal state.
type journalState struct{}

func newJournalState() *journalState { return &journalState{} }

// build returns the journal bytes for the packet with sequence number seq
// whose command list is list; nil means "no journal".
func (j *journalState) build(seq uint16, list []byte) []byte { return nil }

// ack tells the journal the peer has received everything up to seq.
func (j *journalState) ack(seq uint16) {}
