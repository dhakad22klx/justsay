// Package state keeps what a run needs in order to be picked up again after the
// process that started it is gone: the agent state a human-in-the-loop pause
// leaves behind, waiting for an approval that may arrive minutes later, from a
// phone, against a different process.
//
// A file would not survive that. The transcripts in sessions/ and the tokens in
// credentials.json belong to the machine that wrote them, but a paused run has
// to be readable by whichever process handles the reply, so it lives in Redis
// instead.
//
// Nothing here knows what a state entry contains. A caller hands over a value
// to store and a value to decode into, which keeps the shape of agent state in
// the package that understands it and keeps this one about the store: where it
// is, how long an entry lives, and not confusing "never saved" with "saved and
// unreadable".
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

// DefaultTTL bounds how long a paused run is worth resuming. Entries expire
// because a run nobody approved is abandoned, not pending, and without an
// expiry those keys would accumulate in a shared Redis forever.
const DefaultTTL = 24 * time.Hour

// DefaultKeyPrefix namespaces every key this package writes, so a Redis shared
// with anything else cannot be scanned, or flushed, by accident.
const DefaultKeyPrefix = "agent-state"

// Store is what the agent's state lives in. Redis is the implementation here;
// the interface exists because HITL_STATE_STORE names a choice, and a caller
// that holds this can be handed an in-memory store in a test without a Redis to
// talk to.
type Store interface {
	// Put saves value under key, replacing whatever was there.
	Put(ctx context.Context, key string, value any) error

	// Get decodes the entry for key into into. False means there was no entry.
	Get(ctx context.Context, key string, into any) (bool, error)

	// Delete removes the entry for key.
	Delete(ctx context.Context, key string) error

	// Close releases the connections the store holds.
	Close() error
}

// RedisStore is a Store backed by a Redis server. The client pools its own
// connections and is safe to share, so one store serves the whole process.
type RedisStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

var _ Store = (*RedisStore)(nil)

// OpenRedis connects using the REDIS_* and HITL_STATE_TTL entries in .env.
//
// Only REDIS_ADDR is required. REDIS_KEY_PREFIX and HITL_STATE_TTL fall back to
// the defaults above, and an HITL_STATE_TTL of "0" turns expiry off for a
// deployment that would rather keep paused runs until something collects them.
//
// The connection is proven here with a PING rather than left to the first Put.
// go-redis dials lazily, so without this a misconfigured address would surface
// as a failed save halfway through a run, which is the worst moment to discover
// the state store was never reachable.
func OpenRedis(ctx context.Context) (*RedisStore, error) {
	env, err := godotenv.Read(".env")
	if err != nil {
		return nil, fmt.Errorf("read .env: %w", err)
	}

	addr := strings.TrimSpace(env["REDIS_ADDR"])
	if addr == "" {
		return nil, errors.New("REDIS_ADDR is not set in .env")
	}

	ttl, err := parseTTL(env["HITL_STATE_TTL"])
	if err != nil {
		return nil, err
	}

	prefix := strings.TrimSpace(env["REDIS_KEY_PREFIX"])
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}

	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: env["REDIS_PASSWORD"],
		DB:       parseDB(env["REDIS_DB"]),
	})

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close() // Preserve the connection failure, not pool cleanup errors.
		return nil, fmt.Errorf("cannot reach redis at %s: %w", addr, err)
	}

	return &RedisStore{client: client, prefix: prefix, ttl: ttl}, nil
}

// NewRedisStore wraps a client the caller already has. A ttl of zero means the
// entries never expire; an empty prefix means DefaultKeyPrefix.
//
// This is the seam for a test against a throwaway server, and for a caller that
// builds its Redis options from something other than .env. Unlike OpenRedis it
// does not check the connection, since the caller made it.
func NewRedisStore(client *redis.Client, prefix string, ttl time.Duration) *RedisStore {
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}

	return &RedisStore{client: client, prefix: prefix, ttl: ttl}
}

// TTL is how long a freshly written entry lives, zero meaning it does not
// expire.
func (s *RedisStore) TTL() time.Duration { return s.ttl }

// Put encodes value as JSON and saves it under key, replacing whatever was
// there and restarting the entry's clock.
//
// The clock restarts on every write, which is what keeps a run that is still
// being worked on from expiring underneath the person working on it.
func (s *RedisStore) Put(ctx context.Context, key string, value any) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("cannot save state under an empty key")
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cannot encode the state for %s: %w", key, err)
	}

	if err := s.client.Set(ctx, s.key(key), encoded, s.ttl).Err(); err != nil {
		return fmt.Errorf("cannot save the state for %s: %w", key, err)
	}

	return nil
}

// Get decodes the entry for key into into.
//
// The boolean says whether there was an entry at all, which is how a caller
// tells a run that was never paused, or whose pause has expired, from one that
// was saved and cannot be read back. The second is a real failure and must not
// be reported as a run that simply is not there.
func (s *RedisStore) Get(ctx context.Context, key string, into any) (bool, error) {
	if strings.TrimSpace(key) == "" {
		return false, errors.New("cannot read state under an empty key")
	}

	encoded, err := s.client.Get(ctx, s.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot read the state for %s: %w", key, err)
	}

	if err := json.Unmarshal(encoded, into); err != nil {
		return false, fmt.Errorf("cannot decode the state for %s: %w", key, err)
	}

	return true, nil
}

// Delete removes the entry for key.
//
// A key that is already gone is not an error: the caller wanted it absent and
// it is absent. An approval handled twice, or one that lands after the entry
// expired, must not fail on the cleanup.
func (s *RedisStore) Delete(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("cannot delete state under an empty key")
	}

	if err := s.client.Del(ctx, s.key(key)).Err(); err != nil {
		return fmt.Errorf("cannot delete the state for %s: %w", key, err)
	}

	return nil
}

// Close releases the pooled connections.
func (s *RedisStore) Close() error {
	if err := s.client.Close(); err != nil {
		return fmt.Errorf("cannot close the redis connection: %w", err)
	}

	return nil
}

// key namespaces a caller's key with the configured prefix. Every command goes
// through here, so the prefix cannot be forgotten on one path and applied on
// another.
func (s *RedisStore) key(name string) string {
	return s.prefix + ":" + name
}

// parseTTL reads HITL_STATE_TTL, which is a Go duration such as "24h". Blank
// means DefaultTTL and "0" means no expiry.
func parseTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultTTL, nil
	}

	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("HITL_STATE_TTL in .env is not a duration like \"24h\": %w", err)
	}
	if ttl < 0 {
		return 0, fmt.Errorf("HITL_STATE_TTL in .env is negative: %s", raw)
	}

	return ttl, nil
}

// parseDB reads REDIS_DB as the numbered database to select.
//
// A hosted Redis is usually addressed by endpoint and names its database
// something like "database-justsay", which is not an index and is not meant
// as one. That name is accepted and ignored rather than rejected, because the
// endpoint already picked the database; only a number is passed through.
func parseDB(raw string) int {
	db, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}

	return db
}
