-- Token bucket: capacity = limit, refilled at limit tokens per window.
-- KEYS[1] = key
-- ARGV    = now_ms, limit, window_ms
-- Returns {allowed(0|1), remaining(tokens, floored), retry_after_ms}
local now = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local window = tonumber(ARGV[3])

local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil or ts == nil then
  tokens = capacity
  ts = now
end

local elapsed = math.max(0, now - ts)
tokens = math.min(capacity, tokens + elapsed * capacity / window)

local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) * window / capacity)
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', math.max(ts, now))
-- A full bucket is the default state, so the key can expire after one window.
redis.call('PEXPIRE', KEYS[1], window)

return {allowed, math.floor(tokens), retry}
