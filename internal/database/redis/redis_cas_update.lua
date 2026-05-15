-- CAS (Compare-And-Swap) update: only writes fields if the current status matches.
-- KEYS[1] = hash key
-- ARGV[1] = expected status value
-- ARGV[2..N] = field/value pairs to set (same format as HSET)
-- Returns "OK" on success, "CONFLICT" if the current status differs.

local current = redis.call("HGET", KEYS[1], "status")
if current ~= ARGV[1] then
    return "CONFLICT"
end

local fields = {}
for i = 2, #ARGV do
    fields[#fields + 1] = ARGV[i]
end
redis.call("HSET", KEYS[1], unpack(fields))
return "OK"
