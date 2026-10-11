package gateway

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// defaultMessageStorePath is a variable so tests can disable the store.
var defaultMessageStorePath = "/etc/matterbridge/msgstore.db"

const (
	defaultMessageStoreRetentionDays = 180
	msgStorePruneInterval            = time.Hour
)

// msgStore persists the mapping between a message and its bridged copies so
// that edits, deletes and thread replies keep working after a restart and for
// messages older than the in-memory cache holds.
type msgStore struct {
	db  *sql.DB
	log *logrus.Entry
}

// storedMsgID is one bridged copy of a message.
type storedMsgID struct {
	Account   string
	ID        string
	ChannelID string
}

func openMsgStore(path string, log *logrus.Entry) (*msgStore, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes; SQLite allows one writer anyway.
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS msg_map (
	gateway    TEXT    NOT NULL,
	canonical  TEXT    NOT NULL,
	account    TEXT    NOT NULL,
	dest_id    TEXT    NOT NULL,
	channel_id TEXT    NOT NULL,
	created    INTEGER NOT NULL,
	PRIMARY KEY (gateway, canonical, account, channel_id)
);
CREATE INDEX IF NOT EXISTS msg_map_dest ON msg_map (gateway, dest_id);
CREATE INDEX IF NOT EXISTS msg_map_created ON msg_map (created);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &msgStore{db: db, log: log}, nil
}

// save records the bridged copies of the message stored under canonical.
func (s *msgStore) save(gateway, canonical string, ids []storedMsgID) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO msg_map
		(gateway, canonical, account, dest_id, channel_id, created) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, id := range ids {
		if _, err := stmt.Exec(gateway, canonical, id.Account, id.ID, id.ChannelID, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// load returns the bridged copies of the message stored under canonical.
func (s *msgStore) load(gateway, canonical string) ([]storedMsgID, error) {
	rows, err := s.db.Query(`SELECT account, dest_id, channel_id FROM msg_map
		WHERE gateway = ? AND canonical = ?`, gateway, canonical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []storedMsgID
	for rows.Next() {
		var id storedMsgID
		if err := rows.Scan(&id.Account, &id.ID, &id.ChannelID); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// canonicalFor returns the canonical ID of the message that destID is a
// bridged copy of, or "".
func (s *msgStore) canonicalFor(gateway, destID string) (string, error) {
	var canonical string
	err := s.db.QueryRow(`SELECT canonical FROM msg_map WHERE gateway = ? AND dest_id = ? LIMIT 1`,
		gateway, destID).Scan(&canonical)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return canonical, err
}

// prune deletes mappings created before cutoff.
func (s *msgStore) prune(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM msg_map WHERE created < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// pruneLoop deletes mappings older than retention every hour.
func (s *msgStore) pruneLoop(retention time.Duration) {
	for {
		if n, err := s.prune(time.Now().Add(-retention)); err != nil {
			s.log.Errorf("Pruning message store failed: %s", err)
		} else if n > 0 {
			s.log.Debugf("Pruned %d message mappings older than %s", n, retention)
		}
		time.Sleep(msgStorePruneInterval)
	}
}
