package redisstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// exclusiveResource is the namespace-wide lease field (state.UploadNamespaceResource).
// It is spelled out here on purpose: the Lua scripts compare fields against the
// literal, so the wire contract must not silently drift when internal/state adds
// a resource constant.
const exclusiveResource = "namespace:exclusive"

// leaseExpiredMessage is the fixed, token-free lease-lost message. It never
// carries the owner token or any backend detail, so it is safe to log and to
// return to clients.
const leaseExpiredMessage = "document lease expired or was replaced"

// The lease scripts keep every liveness decision inside Redis. TIME is the only
// clock, so a skewed application host can neither steal nor lose a lease, and
// because each acquisition is one script it is all-or-nothing: a conflict leaves
// no field behind. Expired fields are removed before the conflict check so a
// dead holder cannot block a takeover.
//
// Lease values are "<owner>|<expiry_unix_millis>". The scripts parse with
// '^(.-)|(%d+)$', i.e. the last '|' before a digits-only suffix, which round
// trips any owner token, including one that contains '|'.
//
// Every write refreshes the hash TTL to twice the lease. An existing longer TTL
// is preserved: the hash TTL is only a safety net for expired fields, so a short
// lease refreshed inside a namespace must never delete a longer-lived field, and
// an emptied hash deletes itself anyway.
const acquireLeaseScriptSource = `
local key = KEYS[1]
local owner = ARGV[1]
local ttl = tonumber(ARGV[2])
local keyTTL = tonumber(ARGV[3])
local now = redis.call('TIME')
now = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)

local live = {}
local entries = redis.call('HGETALL', key)
for index = 1, #entries, 2 do
  local field = entries[index]
  local holder, expiry = string.match(entries[index + 1], '^(.-)|(%d+)$')
  local expiresAt = 0
  if holder ~= nil then
    expiresAt = tonumber(expiry)
  end
  if holder == nil or expiresAt <= now then
    redis.call('HDEL', key, field)
  else
    live[field] = holder
  end
end

local requested = {}
local exclusiveRequested = false
for index = 4, #ARGV do
  local resource = ARGV[index]
  requested[#requested + 1] = resource
  if resource == 'namespace:exclusive' then
    exclusiveRequested = true
  end
end

local busy = false
if exclusiveRequested then
  for _, holder in pairs(live) do
    if holder ~= owner then
      busy = true
      break
    end
  end
else
  local exclusiveHolder = live['namespace:exclusive']
  if exclusiveHolder ~= nil and exclusiveHolder ~= owner then
    busy = true
  else
    for _, resource in ipairs(requested) do
      local holder = live[resource]
      if holder ~= nil and holder ~= owner then
        busy = true
        break
      end
    end
  end
end
if busy then
  return 0
end

local value = owner .. '|' .. string.format('%d', now + ttl)
for _, resource in ipairs(requested) do
  redis.call('HSET', key, resource, value)
end
local currentTTL = redis.call('PTTL', key)
if currentTTL > keyTTL then
  keyTTL = currentTTL
end
redis.call('PEXPIRE', key, string.format('%d', keyTTL))
return 1
`

const renewLeaseScriptSource = `
local key = KEYS[1]
local owner = ARGV[1]
local ttl = tonumber(ARGV[2])
local keyTTL = tonumber(ARGV[3])
if #ARGV < 4 then
  return 0
end
local now = redis.call('TIME')
now = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)

for index = 4, #ARGV do
  local current = redis.call('HGET', key, ARGV[index])
  if not current then
    return 0
  end
  local holder, expiry = string.match(current, '^(.-)|(%d+)$')
  if holder == nil or holder ~= owner then
    return 0
  end
  if tonumber(expiry) <= now then
    return 0
  end
end

local value = owner .. '|' .. string.format('%d', now + ttl)
for index = 4, #ARGV do
  redis.call('HSET', key, ARGV[index], value)
end
local currentTTL = redis.call('PTTL', key)
if currentTTL > keyTTL then
  keyTTL = currentTTL
end
redis.call('PEXPIRE', key, string.format('%d', keyTTL))
return 1
`

const releaseLeaseScriptSource = `
local key = KEYS[1]
local owner = ARGV[1]
for index = 2, #ARGV do
  local current = redis.call('HGET', key, ARGV[index])
  if current then
    local holder = string.match(current, '^(.-)|%d+$')
    if holder == owner then
      redis.call('HDEL', key, ARGV[index])
    end
  end
end
if redis.call('HLEN', key) == 0 then
  redis.call('DEL', key)
end
return 1
`

const inspectLeaseScriptSource = `
local current = redis.call('HGET', KEYS[1], ARGV[1])
local now = redis.call('TIME')
now = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local clock = string.format('%d', now)
if not current then
  return {'', clock}
end
return {current, clock}
`

var (
	acquireLeaseScript = redis.NewScript(acquireLeaseScriptSource)
	renewLeaseScript   = redis.NewScript(renewLeaseScriptSource)
	releaseLeaseScript = redis.NewScript(releaseLeaseScriptSource)
	inspectLeaseScript = redis.NewScript(inspectLeaseScriptSource)
)

// DocumentLock is the Redis lease backend for document resources.
//
// One namespace maps to one Redis hash; a field is one leased resource and its
// value is "<owner>|<expiry_unix_millis>". Every call runs exactly one Lua
// script, so cleanup, conflict detection and the write are atomic, and every
// liveness decision uses the Redis server clock instead of the caller's clock.
// All methods are safe for concurrent use.
type DocumentLock struct {
	settings state.DocumentLockSettings

	mutex  sync.Mutex
	redis  *redis.Client
	closed bool
}

// Compile-time interface checks.
var _ state.DocumentLocker = (*DocumentLock)(nil)

// NewDocumentLock builds the Redis lease backend from explicit settings. It
// validates them exactly like the other backends and never dials: Redis may be
// unreachable while the service starts, and every method then reports
// *state.LockUnavailable instead of blocking startup. WithRedisClient injects an
// already-configured client (tests); the injected client is closed by Close.
func NewDocumentLock(settings state.DocumentLockSettings, options ...Option) (*DocumentLock, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	resolved := clientOptions{}
	for _, option := range options {
		option(&resolved)
	}
	return &DocumentLock{settings: settings, redis: resolved.redisClient}, nil
}

// leaseKey returns the per-namespace lease hash. A single hash with one hash tag
// keeps every field of a namespace in one Cluster slot, so the scripts never
// cross slots.
func (d *DocumentLock) leaseKey(namespace string) string {
	sum := sha256.Sum256([]byte(namespace))
	return d.settings.Prefix + ":{" + hex.EncodeToString(sum[:])[:32] + "}:leases"
}

// lockOwnerDigest mirrors the lease read model: the first 16 lowercase hex
// characters of sha256(owner). The raw token never leaves the backend.
func lockOwnerDigest(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])[:16]
}

// leaseMillis converts a lease duration to the millisecond resolution stored in
// Redis, rounding up so a positive TTL never becomes "already expired".
func leaseMillis(ttl time.Duration) int64 {
	millis := int64(ttl / time.Millisecond)
	if ttl%time.Millisecond != 0 || millis < 1 {
		millis++
	}
	return millis
}

// connection lazily creates the go-redis client, mirroring Client.connection.
func (d *DocumentLock) connection() (*redis.Client, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.closed {
		return nil, state.NewLockUnavailable()
	}
	if d.redis != nil {
		return d.redis, nil
	}
	if d.settings.URL == "" {
		return nil, state.NewLockUnavailable()
	}
	options, err := redisOptionsFromURL(d.settings.URL, d.settings.TimeoutSeconds)
	if err != nil {
		return nil, state.NewLockUnavailable()
	}
	d.redis = redis.NewClient(options)
	return d.redis, nil
}

// Acquire leases every resource atomically. Re-acquiring a resource already
// owned by owner refreshes its expiry. A live foreign holder returns
// *state.LockBusy, and a backend failure returns *state.LockUnavailable.
func (d *DocumentLock) Acquire(ctx context.Context, namespace string, resources []string, owner string, ttl time.Duration) error {
	if ttl <= 0 {
		return state.NewLockUnavailable()
	}
	connection, err := d.connection()
	if err != nil {
		return err
	}
	millis := leaseMillis(ttl)
	arguments := make([]any, 0, 3+len(resources))
	arguments = append(arguments, owner, millis, millis*2)
	for _, resource := range resources {
		arguments = append(arguments, resource)
	}
	acquired, err := acquireLeaseScript.Run(ctx, connection, []string{d.leaseKey(namespace)}, arguments...).Int64()
	if err != nil {
		return state.NewLockUnavailable()
	}
	if acquired != 1 {
		return state.NewLockBusy()
	}
	return nil
}

// Renew extends every resource only when all of them are still live and held by
// owner; otherwise nothing is written and *state.LeaseLost is returned.
func (d *DocumentLock) Renew(ctx context.Context, namespace string, resources []string, owner string, ttl time.Duration) error {
	if ttl <= 0 {
		return state.NewLockUnavailable()
	}
	connection, err := d.connection()
	if err != nil {
		return err
	}
	millis := leaseMillis(ttl)
	arguments := make([]any, 0, 3+len(resources))
	arguments = append(arguments, owner, millis, millis*2)
	for _, resource := range resources {
		arguments = append(arguments, resource)
	}
	renewed, err := renewLeaseScript.Run(ctx, connection, []string{d.leaseKey(namespace)}, arguments...).Int64()
	if err != nil {
		return state.NewLockUnavailable()
	}
	if renewed != 1 {
		return &state.LeaseLost{UploadConflict: state.UploadConflict{Message: leaseExpiredMessage}}
	}
	return nil
}

// Release removes only the resources still owned by owner and is idempotent: a
// missing resource or a foreign holder is not an error.
func (d *DocumentLock) Release(ctx context.Context, namespace string, resources []string, owner string) error {
	connection, err := d.connection()
	if err != nil {
		return err
	}
	arguments := make([]any, 0, 1+len(resources))
	arguments = append(arguments, owner)
	for _, resource := range resources {
		arguments = append(arguments, resource)
	}
	if _, err := releaseLeaseScript.Run(ctx, connection, []string{d.leaseKey(namespace)}, arguments...).Int64(); err != nil {
		return state.NewLockUnavailable()
	}
	return nil
}

// Inspect reports one resource's live lease for display. It returns a zero
// state (and no error) when the resource is free or its lease has expired, and
// it never exposes the raw owner token.
func (d *DocumentLock) Inspect(ctx context.Context, namespace, resource string) (state.LockState, error) {
	connection, err := d.connection()
	if err != nil {
		return state.LockState{}, err
	}
	values, err := inspectLeaseScript.Run(ctx, connection, []string{d.leaseKey(namespace)}, resource).Slice()
	if err != nil {
		return state.LockState{}, state.NewLockUnavailable()
	}
	if len(values) != 2 {
		return state.LockState{}, state.NewLockUnavailable()
	}
	raw, ok := cacheText(values[0])
	if !ok {
		return state.LockState{}, state.NewLockUnavailable()
	}
	clock, ok := cacheText(values[1])
	if !ok {
		return state.LockState{}, state.NewLockUnavailable()
	}
	now, err := strconv.ParseInt(clock, 10, 64)
	if err != nil {
		return state.LockState{}, state.NewLockUnavailable()
	}
	if raw == "" {
		return state.LockState{}, nil
	}
	owner, expiry, ok := parseLeaseValue(raw)
	if !ok {
		// A malformed value cannot prove ownership; the display projection says
		// "not held" instead of failing a read path.
		return state.LockState{}, nil
	}
	remaining := expiry - now
	if remaining <= 0 {
		return state.LockState{}, nil
	}
	return state.LockState{
		OwnerDigest: lockOwnerDigest(owner),
		TTL:         time.Duration(remaining) * time.Millisecond,
	}, nil
}

// parseLeaseValue splits "<owner>|<expiry_unix_millis>", matching the Lua
// pattern '^(.-)|(%d+)$': the split point is the last '|' followed only by
// digits.
func parseLeaseValue(raw string) (string, int64, bool) {
	index := strings.LastIndex(raw, "|")
	if index < 0 {
		return "", 0, false
	}
	expiryText := raw[index+1:]
	if expiryText == "" {
		return "", 0, false
	}
	for position := 0; position < len(expiryText); position++ {
		if expiryText[position] < '0' || expiryText[position] > '9' {
			return "", 0, false
		}
	}
	expiry, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return raw[:index], expiry, true
}

// Ping checks backend connectivity for readiness probes.
func (d *DocumentLock) Ping(ctx context.Context) error {
	connection, err := d.connection()
	if err != nil {
		return err
	}
	if err := connection.Ping(ctx).Err(); err != nil {
		return state.NewLockUnavailable()
	}
	return nil
}

// Close releases the Redis client. It is idempotent and a never-used backend
// closes without dialing.
func (d *DocumentLock) Close() error {
	d.mutex.Lock()
	if d.closed {
		d.mutex.Unlock()
		return nil
	}
	d.closed = true
	connection := d.redis
	d.redis = nil
	d.mutex.Unlock()
	if connection == nil {
		return nil
	}
	if err := connection.Close(); err != nil {
		return state.NewLockUnavailable()
	}
	return nil
}
