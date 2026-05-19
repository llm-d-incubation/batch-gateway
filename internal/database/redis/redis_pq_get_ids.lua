-- Copyright 2026 The llm-d Authors

-- Licensed under the Apache License, Version 2.0 (the "License");
-- you may not use this file except in compliance with the License.
-- You may obtain a copy of the License at

--     http://www.apache.org/licenses/LICENSE-2.0

-- Unless required by applicable law or agreed to in writing, software
-- distributed under the License is distributed on an "AS IS" BASIS,
-- WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
-- See the License for the specific language governing permissions and
-- limitations under the License.

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
