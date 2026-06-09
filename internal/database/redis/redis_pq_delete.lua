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

-- PQDelete: removes exactly one member from the priority queue sorted set
-- matching both the given score and job ID.
-- KEYS[1] = sorted set key (priority queue)
-- ARGV[1] = score (SLO as UnixMicro string)
-- ARGV[2] = target job ID
-- Returns: number of members removed (0 or 1)

local members = redis.call("ZRANGEBYSCORE", KEYS[1], ARGV[1], ARGV[1])
for i = 1, #members do
    local decoded = cjson.decode(members[i])
    if decoded and decoded.id == ARGV[2] then
        return redis.call("ZREM", KEYS[1], members[i])
    end
end
return 0
