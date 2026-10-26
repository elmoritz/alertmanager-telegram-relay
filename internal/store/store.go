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
	GroupKey  string    `json:"groupKey"`
	ChatID    int64     `json:"chatId"`
	MessageID int       `json:"messageId"`
	OpenedAt  time.Time `json:"openedAt"`
	// LastSeen is when the last notification for this incident arrived.
	LastSeen time.Time `json:"lastSeen"`
	// Firing holds the keys of the alerts firing at the last notification.
	Firing []string `json:"firing"`
	// TextHash identifies the last text sent or edited into the message.
	TextHash string `json:"textHash"`
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
		for _, b := range [][]byte{bucketIncidents, bucketQueue} {
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

// CountIncidents returns the number of open incidents.
func (s *Store) CountIncidents() (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketIncidents).Stats().KeyN
		return nil
	})
	return n, err
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
