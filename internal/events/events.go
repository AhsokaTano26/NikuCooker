// Package events carries live updates from the core to whatever is watching.
//
// One sequence number, one buffer, one fan-out. The sequence is what makes the
// stream resumable: a client that saw 1043 and reconnects asking for 1044 gets
// exactly what it missed, in order, with no duplicates and no gaps. Everything
// else here exists to make that guarantee hold or to say honestly when it
// cannot.
//
// It is deliberately not a durable log. The sequence resets with the process,
// and a client whose position has fallen out of the buffer is told to refetch
// over REST rather than given a silently incomplete replay — a gap the client
// does not know about is the failure mode that makes a live-updating UI
// untrustworthy, because it looks like the server has nothing more to say.
package events

import (
	"fmt"
	"sync"
	"time"
)

// Type is an event's name, from the catalog in docs/events.md §4.
type Type string

// The event catalog.
const (
	// TypeHello is sent once on connect, before any replay.
	TypeHello Type = "hello"

	// TypeResyncRequired tells a client to discard its state and refetch.
	TypeResyncRequired Type = "resync.required"

	TypeJobStatus     Type = "job.status"
	TypeJobProgress   Type = "job.progress"
	TypeStageStatus   Type = "stage.status"
	TypeStageProgress Type = "stage.progress"

	TypeProjectUpdated   Type = "project.updated"
	TypeProjectCreated   Type = "project.created"
	TypeProjectDeleted   Type = "project.deleted"
	TypeSegmentUpdated   Type = "segment.updated"
	TypeSegmentsReplaced Type = "segments.replaced"

	TypeModelProgress Type = "model.progress"
	TypeLog           Type = "log"
	TypeWorkerStatus  Type = "worker.status"
	TypeSystemStats   Type = "system.stats"
)

// Event is the envelope every client receives.
type Event struct {
	// Seq is monotonic per process and increases by exactly one per event.
	Seq int64 `json:"seq"`

	Type Type      `json:"type"`
	TS   time.Time `json:"ts"`

	ProjectID string `json:"project_id,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	Stage     string `json:"stage,omitempty"`

	Data map[string]any `json:"data"`
}

// DefaultBufferSize is how many events are retained for replay.
//
// Sized so that a browser tab that was backgrounded for a few minutes still
// resumes rather than resyncing: at the two-per-second progress cap, 4096 events
// is roughly half an hour of a busy run. Memory is a few hundred kilobytes.
const DefaultBufferSize = 4096

// Bus fans events out to subscribers and retains a replay window.
type Bus struct {
	mu sync.Mutex

	seq    int64
	buffer []Event
	// start is the index in buffer of the oldest retained event.
	start int

	subscribers map[int64]*Subscription
	nextID      int64

	// now is injectable so a test can assert on timestamps without sleeping.
	now func() time.Time
}

// NewBus builds a bus with a replay window.
func NewBus(capacity int) *Bus {
	if capacity <= 0 {
		capacity = DefaultBufferSize
	}
	return &Bus{
		buffer:      make([]Event, 0, capacity),
		subscribers: map[int64]*Subscription{},
		now:         time.Now,
	}
}

// Emit assigns the event a sequence number and timestamp, then delivers it.
//
// The sequence is assigned here, under the same lock that appends to the buffer,
// so that a client can never observe a sequence number whose event is not yet
// replayable. Assigning them separately would leave a window in which a
// reconnect asks for an event that exists in neither the buffer nor the
// subscriber's hands.
func (b *Bus) Emit(event Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	event.Seq = b.seq
	if event.TS.IsZero() {
		event.TS = b.now().UTC()
	}
	if event.Data == nil {
		// Never null: a client that has to null-check every payload will
		// eventually forget, and the one place it forgets is the one that
		// crashes.
		event.Data = map[string]any{}
	}

	b.append(event)

	for _, subscription := range b.subscribers {
		subscription.deliver(event)
	}

	return event
}

// append adds an event to the replay window, evicting the oldest if full.
func (b *Bus) append(event Event) {
	if len(b.buffer) < cap(b.buffer) {
		b.buffer = append(b.buffer, event)
		return
	}

	// Full: overwrite the oldest in place. A ring keeps the allocation count at
	// zero for the lifetime of the process, which matters because this runs
	// while a job is executing.
	b.buffer[b.start] = event
	b.start = (b.start + 1) % len(b.buffer)
}

// CurrentSeq reports the most recently assigned sequence number.
func (b *Bus) CurrentSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// OldestSeq reports the oldest sequence number still replayable.
//
// Zero when nothing has been evicted yet, which a client reads as "everything
// since the beginning is available".
func (b *Bus) OldestSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.oldestSeqLocked()
}

// oldestSeqLocked is OldestSeq for callers already holding the lock.
//
// sync.Mutex is not reentrant, so calling the exported form from inside a
// critical section deadlocks the whole bus — and because the bus lock is held
// by the emitter too, it does not hang one request; it stops every event in the
// process. The pair exists so that mistake is not expressible.
func (b *Bus) oldestSeqLocked() int64 {
	if len(b.buffer) < cap(b.buffer) {
		return 0
	}
	return b.buffer[b.start].Seq
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

// Subscription is one client's view of the stream.
type Subscription struct {
	bus *Bus
	id  int64

	ch chan Event

	// overflowed records that events were dropped because the client could not
	// keep up. The stream handler turns it into a resync instruction rather
	// than letting the client believe it is caught up.
	overflowed bool

	closed bool
	once   sync.Once
}

// SubscriberBuffer is how many events a client may fall behind by.
//
// A client that has fallen this far behind is not going to catch up: the
// alternative to dropping it is unbounded memory growth in the server, which
// would take the whole process down rather than one stalled browser tab.
const SubscriberBuffer = 256

// Subscribe returns a subscription positioned after `since`.
//
// The second return value reports whether the requested position is still
// replayable. When it is false the caller must send a resync instruction: the
// events between `since` and the buffer's start are gone, and a stream that
// simply started from the oldest retained event would leave the client with a
// hole it cannot detect.
//
// A `since` of zero means "from now", and is always replayable — that is a
// fresh client with no state to be missing anything.
func (b *Bus) Subscribe(since int64) (*Subscription, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subscription := &Subscription{
		bus: b,
		id:  b.nextID,
		ch:  make(chan Event, SubscriberBuffer),
	}
	b.nextID++
	b.subscribers[subscription.id] = subscription

	if since <= 0 {
		return subscription, true
	}

	if since > b.seq {
		// The client claims to have seen events this process never emitted,
		// which means it is talking to a restarted server. Its state is from a
		// previous life and cannot be reconciled by replay.
		return subscription, false
	}

	oldest := b.oldestSeqLocked()
	if oldest != 0 && since < oldest-1 {
		return subscription, false
	}

	for _, event := range b.replayFrom(since) {
		subscription.deliver(event)
	}
	return subscription, true
}

// replayFrom returns the retained events after a sequence number.
func (b *Bus) replayFrom(since int64) []Event {
	out := make([]Event, 0, len(b.buffer))
	for i := 0; i < len(b.buffer); i++ {
		index := (b.start + i) % len(b.buffer)
		if b.buffer[index].Seq > since {
			out = append(out, b.buffer[index])
		}
	}
	return out
}

// Events returns the channel of events for this subscription.
//
// The channel is closed when the subscription is closed or when the client has
// fallen too far behind. Callers must read Overflowed after the channel closes
// to tell the two apart.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Overflowed reports whether events were dropped for this client.
func (s *Subscription) Overflowed() bool {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.overflowed
}

// deliver hands an event to a subscriber.
//
// Called with the bus lock held, so it must never block. A full buffer means the
// client is not keeping up, and the only honest responses are to drop the client
// or to grow memory without bound. Dropping is the one that keeps the server
// alive for everyone else.
func (s *Subscription) deliver(event Event) {
	if s.closed {
		return
	}

	select {
	case s.ch <- event:
	default:
		s.overflowed = true
		s.close()
	}
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.close()
}

// close ends the subscription. Called with the bus lock held.
func (s *Subscription) close() {
	s.once.Do(func() {
		s.closed = true
		delete(s.bus.subscribers, s.id)
		close(s.ch)
	})
}

// Subscribers reports how many clients are connected.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// Close ends every subscription, for shutdown.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, subscription := range b.subscribers {
		subscription.close()
	}
}

// ---------------------------------------------------------------------------
// Convenience
// ---------------------------------------------------------------------------

// Emitter is the subset of the bus that producers use.
//
// It exists so that code which only emits — a pipeline observer, a job runner —
// cannot accidentally subscribe, read the sequence, or close someone else's
// stream.
type Emitter interface {
	Emit(event Event) Event
	CurrentSeq() int64
}

// New builds an event of a type with no scope.
func New(eventType Type) Event {
	return Event{Type: eventType, Data: map[string]any{}}
}

// ForProject scopes an event to a project.
func (e Event) ForProject(projectID string) Event {
	e.ProjectID = projectID
	return e
}

// ForJob scopes an event to a job.
func (e Event) ForJob(jobID string) Event {
	e.JobID = jobID
	return e
}

// ForStage scopes an event to a stage.
func (e Event) ForStage(stage string) Event {
	e.Stage = stage
	return e
}

// With attaches a payload field.
func (e Event) With(key string, value any) Event {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data[key] = value
	return e
}

// String renders an event for a log line.
func (e Event) String() string {
	return fmt.Sprintf("%d %s", e.Seq, e.Type)
}
