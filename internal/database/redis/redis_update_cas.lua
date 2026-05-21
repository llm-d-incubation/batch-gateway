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

-- CAS (Compare-And-Swap) update: only writes fields if the current status matches.
-- KEYS[1] = hash key
-- ARGV[1] = expected status value
-- ARGV[2..N] = field/value pairs to set (same format as HSET)
-- Returns "OK" on success, "NOT_FOUND" if the key does not exist,
-- "CONFLICT" if the current status differs from the expected value.

local current = redis.call("HGET", KEYS[1], "status")
if current == false then
    return "NOT_FOUND"
end
if current ~= ARGV[1] then
    return "CONFLICT"
end

local fields = {}
for i = 2, #ARGV do
    fields[#fields + 1] = ARGV[i]
end
redis.call("HSET", KEYS[1], unpack(fields))
return "OK"
