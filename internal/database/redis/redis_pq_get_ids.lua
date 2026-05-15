-- PQGetIDs: returns all job IDs from the priority queue sorted set.
-- KEYS[1] = sorted set key (priority queue)
-- Returns a flat array of job IDs extracted from the JSON members.

local ids = {}
local cursor = "0"
repeat
    local result = redis.call("ZSCAN", KEYS[1], cursor, "COUNT", 100)
    cursor = result[1]
    local members = result[2]
    -- ZSCAN returns [member, score, member, score, ...]
    for i = 1, #members, 2 do
        local member = members[i]
        local decoded = cjson.decode(member)
        if decoded and decoded.id then
            ids[#ids + 1] = decoded.id
        end
    end
until cursor == "0"
return ids
