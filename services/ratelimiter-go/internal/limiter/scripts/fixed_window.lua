-- Fixed window counter.
-- KEYS[1] = base key
-- ARGV    = now_ms, limit, window_ms
-- Returns {allowed(0|1), remaining, retry_after_ms}
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])

local window_id = math.floor(now / window)
local key = KEYS[1] .. ':' .. window_id

local count = redis.call('INCR', key)
if count == 1 then
  redis.call('PEXPIRE', key, window)
end

if count <= limit then
  return {1, limit - count, 0}
end
return {0, 0, (window_id + 1) * window - now}
