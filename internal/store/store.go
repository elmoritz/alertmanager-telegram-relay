// Package store persists the relay's state in a single bbolt file: open
// incidents (which Telegram message belongs to which Alertmanager group) and
// the queue of notifications not yet delivered to Telegram.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketIncidents = []byte("incidents")
	bucketQueue     = []byte("queue")
	bucketActions   = []byte("actions")
)

// FileName is the state file inside STATE_DIR.
const FileName = "relay.db"

// Store is safe for concurrent use.
type Store struct {
	db *bolt.DB
}

// Incident is one Alertmanager group, from its first firing notification until
// it resolves, and the Telegram message that shows it.
type Incident struct {
	GroupKey  string `json:"groupKey"`
	ChatID    int64  `json:"chatId"`
	MessageID int    `json:"messageId"`
	// ThreadID is the forum topic the message is in; 0 for none.
	ThreadID int       `json:"threadId,omitempty"`
	OpenedAt time.Time `json:"openedAt"`
	// LastSeen is when the last notification for this incident arrived.
	LastSeen time.Time `json:"lastSeen"`
	// Firing holds the keys of the alerts firing at the last notification.
	Firing []string `json:"firing"`
	// TextHash identifies the last text and keyboard sent or edited into the
	// message.
	TextHash string `json:"textHash"`

	// GroupLabels are the notification's group labels: the only source of
	// silence matchers (SECURITY.md §3).
	GroupLabels map[string]string `json:"groupLabels,omitempty"`
	// Last is the last notification, to re-render the message after a
	// button press.
	Last json.RawMessage `json:"last,omitempty"`
	// Buttons are the callback buttons on the message's current keyboard. A
	// press is only acted on if its action is listed here.
	Buttons []ButtonRef `json:"buttons,omitempty"`
	// ButtonsIssued is when the current Buttons were created.
	ButtonsIssued time.Time `json:"buttonsIssued,omitzero"`
	// Silence is the silence made through this message's buttons, if any.
	Silence *Silence `json:"silence,omitempty"`
	// Ack is who said "I'm on it", if anyone.
	Ack *Ack `json:"ack,omitempty"`
	// Escalations counts the escalation replies sent so far; LastEscalation
	// is when the latest went out, and EscalationMessageIDs are their
	// messages, on which a reaction acknowledges like one on the incident.
	Escalations          int       `json:"escalations,omitempty"`
	LastEscalation       time.Time `json:"lastEscalation,omitzero"`
	EscalationMessageIDs []int     `json:"escalationMessageIds,omitempty"`
	// Reaction is the bot's current reaction on the message ("" for none).
	Reaction string `json:"reaction,omitempty"`
}

// Ack is an "I'm on it" from an admin. It changes only the relay's state.
type Ack struct {
	ByID   int64     `json:"byId"`
	ByName string    `json:"byName"`
	At     time.Time `json:"at"`
}

// HasButtonKind reports whether the message's keyboard has a button of kind.
func (inc *Incident) HasButtonKind(kind string) bool {
	for _, b := range inc.Buttons {
		if b.Kind == kind {
			return true
		}
	}
	return false
}

// ButtonRef is a callback button on an incident's keyboard.
type ButtonRef struct {
	ActionID string `json:"actionId"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
}

// HasButton reports whether actionID is on the incident's current keyboard.
func (inc *Incident) HasButton(actionID string) bool {
	for _, b := range inc.Buttons {
		if b.ActionID == actionID {
			return true
		}
	}
	return false
}

// ButtonIDs returns the action IDs of the current keyboard.
func (inc *Incident) ButtonIDs() []string {
	var ids []string
	for _, b := range inc.Buttons {
		ids = append(ids, b.ActionID)
	}
	return ids
}

// Silence is a silence created from Telegram.
type Silence struct {
	ID     string    `json:"id"`
	Until  time.Time `json:"until"`
	ByID   int64     `json:"byId"`
	ByName string    `json:"byName"`
}

// Action kinds.
const (
	ActionSilence   = "silence"
	ActionUnsilence = "unsilence"
	ActionAck       = "ack"
	ActionRelease   = "release"
)

// Action is what a callback button does. Buttons carry only the action's
// random ID; everything the relay acts on is stored here (SECURITY.md §3).
type Action struct {
	ID        string        `json:"id"`
	Kind      string        `json:"kind"`
	GroupKey  string        `json:"groupKey"`
	Duration  time.Duration `json:"duration,omitempty"`  // silence
	SilenceID string        `json:"silenceId,omitempty"` // unsilence
	ExpiresAt time.Time     `json:"expiresAt"`
}

// Open opens (creating if needed) the state file in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening state file: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketIncidents, bucketQueue, bucketActions} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialising state file: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the state file.
func (s *Store) Close() error { return s.db.Close() }

// Ping reports whether the store is open and readable.
func (s *Store) Ping() error {
	return s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketIncidents) == nil {
			return errors.New("incidents bucket missing")
		}
		return nil
	})
}

// incidentKey is the bucket key for a group key. bbolt limits keys to 32 KiB
// and group keys come from the payload, so long ones are hashed.
func incidentKey(groupKey string) []byte {
	if len(groupKey) <= 1024 {
		return []byte(groupKey)
	}
	sum := sha256.Sum256([]byte(groupKey))
	return append([]byte("sha256:"), sum[:]...)
}

// Incident returns the open incident for groupKey, or nil if there is none.
func (s *Store) Incident(groupKey string) (*Incident, error) {
	var inc *Incident
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketIncidents).Get(incidentKey(groupKey))
		if v == nil {
			return nil
		}
		inc = new(Incident)
		return json.Unmarshal(v, inc)
	})
	if err != nil {
		return nil, fmt.Errorf("reading incident: %w", err)
	}
	return inc, nil
}

// PutIncident creates or replaces an incident.
func (s *Store) PutIncident(inc *Incident) error {
	v, err := json.Marshal(inc)
	if err != nil {
		return err
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketIncidents).Put(incidentKey(inc.GroupKey), v)
	})
	if err != nil {
		return fmt.Errorf("writing incident: %w", err)
	}
	return nil
}

// DeleteIncident closes an incident. Deleting a missing one is not an error.
func (s *Store) DeleteIncident(groupKey string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketIncidents).Delete(incidentKey(groupKey))
	})
	if err != nil {
		return fmt.Errorf("deleting incident: %w", err)
	}
	return nil
}

// ExpireIncidents deletes incidents last seen before cutoff and returns how
// many were deleted.
func (s *Store) ExpireIncidents(cutoff time.Time) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketIncidents)
		var expired [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var inc Incident
			// An unreadable record cannot be used either; drop it.
			if err := json.Unmarshal(v, &inc); err != nil || inc.LastSeen.Before(cutoff) {
				expired = append(expired, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		// Deleting while iterating can skip keys in bbolt, hence two passes.
		for _, k := range expired {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		n = len(expired)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("expiring incidents: %w", err)
	}
	return n, nil
}

// Incidents returns the open incidents for which match is true. The
// returned copies may be stale by the time they are used: re-read an
// incident under its group's lock before changing it.
func (s *Store) Incidents(match func(*Incident) bool) ([]*Incident, error) {
	var out []*Incident
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketIncidents).ForEach(func(_, v []byte) error {
			inc := &Incident{}
			if json.Unmarshal(v, inc) == nil && match(inc) {
				out = append(out, inc)
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing incidents: %w", err)
	}
	return out, nil
}

// CountIncidents returns the number of open incidents.
func (s *Store) CountIncidents() (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketIncidents).Stats().KeyN
		return nil
	})
	return n, err
}

// PutActions stores actions.
func (s *Store) PutActions(actions ...*Action) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketActions)
		for _, a := range actions {
			v, err := json.Marshal(a)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(a.ID), v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing actions: %w", err)
	}
	return nil
}

// Action returns the action with the given ID, or nil if there is none or it
// has expired.
func (s *Store) Action(id string, now time.Time) (*Action, error) {
	if id == "" || len(id) > 64 {
		return nil, nil
	}
	var a *Action
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketActions).Get([]byte(id))
		if v == nil {
			return nil
		}
		a = new(Action)
		return json.Unmarshal(v, a)
	})
	if err != nil {
		return nil, fmt.Errorf("reading action: %w", err)
	}
	if a != nil && !now.Before(a.ExpiresAt) {
		return nil, nil
	}
	return a, nil
}

// DeleteActions removes actions; missing IDs are ignored.
func (s *Store) DeleteActions(ids ...string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketActions)
		for _, id := range ids {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("deleting actions: %w", err)
	}
	return nil
}

// ExpireActions deletes actions that expired before now.
func (s *Store) ExpireActions(now time.Time) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketActions)
		var expired [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var a Action
			if err := json.Unmarshal(v, &a); err != nil || !now.Before(a.ExpiresAt) {
				expired = append(expired, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range expired {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		n = len(expired)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("expiring actions: %w", err)
	}
	return n, nil
}

// Job is a queued notification for one chat.
type Job struct {
	Seq     uint64
	ChatID  int64
	Payload []byte
}

// queue keys: 8-byte chat ID, then 8-byte sequence, both big-endian, so a
// cursor seek on the chat prefix yields that chat's jobs in arrival order.
func queueKey(chatID int64, seq uint64) []byte {
	k := make([]byte, 16)
	binary.BigEndian.PutUint64(k, uint64(chatID))
	binary.BigEndian.PutUint64(k[8:], seq)
	return k
}

// Enqueue durably appends payload to chatID's queue.
func (s *Store) Enqueue(chatID int64, payload []byte) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketQueue)
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		return b.Put(queueKey(chatID, seq), payload)
	})
	if err != nil {
		return fmt.Errorf("queueing notification: %w", err)
	}
	return nil
}

// Next returns the oldest job queued for chatID, or nil if there is none.
func (s *Store) Next(chatID int64) (*Job, error) {
	var job *Job
	err := s.db.View(func(tx *bolt.Tx) error {
		prefix := queueKey(chatID, 0)[:8]
		k, v := tx.Bucket(bucketQueue).Cursor().Seek(prefix)
		if k == nil || len(k) != 16 || string(k[:8]) != string(prefix) {
			return nil
		}
		job = &Job{Seq: binary.BigEndian.Uint64(k[8:]), ChatID: chatID, Payload: append([]byte(nil), v...)}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading queue: %w", err)
	}
	return job, nil
}

// Done removes a job from the queue.
func (s *Store) Done(job *Job) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketQueue).Delete(queueKey(job.ChatID, job.Seq))
	})
	if err != nil {
		return fmt.Errorf("removing job: %w", err)
	}
	return nil
}

// QueueChats returns the chats that have queued jobs.
func (s *Store) QueueChats() ([]int64, error) {
	var chats []int64
	err := s.db.View(func(tx *bolt.Tx) error {
		var last []byte
		return tx.Bucket(bucketQueue).ForEach(func(k, _ []byte) error {
			if len(k) == 16 && string(k[:8]) != string(last) {
				last = k[:8]
				chats = append(chats, int64(binary.BigEndian.Uint64(k[:8])))
			}
			return nil
		})
	})
	return chats, err
}

// QueueLen returns the number of queued jobs across all chats.
func (s *Store) QueueLen() (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketQueue).Stats().KeyN
		return nil
	})
	return n, err
}
