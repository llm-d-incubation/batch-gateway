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

-- PQDequeueAndClaim: atomically pop the highest-priority (lowest score) member
-- from the priority queue and record an in-flight claim for it.
-- KEYS[1] = priority queue sorted set
-- KEYS[2] = in-flight hash
-- ARGV[1] = in-flight entry JSON (marshaled by the caller)
-- Returns the popped member JSON, or false when the queue is empty.

local popped = redis.call("ZPOPMIN", KEYS[1])
if #popped == 0 then
    return false
end
local member = popped[1]
local ok, decoded = pcall(cjson.decode, member)
if not ok or type(decoded) ~= "table" or decoded.id == nil then
    -- Corrupt member: script effects are not rolled back on error, so raising
    -- would eat the popped member. Return it unclaimed; the caller logs it.
    return member
end
redis.call("HSET", KEYS[2], decoded.id, ARGV[1])
return member
