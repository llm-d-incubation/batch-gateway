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

-- Store lua script.

-- Parse inputs.
local hashKey = KEYS[1]
local fieldVer = ARGV[1]
local fieldId = ARGV[2]
local fieldExpiry = tonumber(ARGV[3])
local fieldTags = ARGV[4]
local fieldStatus = ARGV[5]
local fieldSpec = ARGV[6]
local ttl = tonumber(ARGV[7])

-- Add the hash key.
redis.call('HSET', hashKey, "ver", fieldVer, "id", fieldId, "expiry", fieldExpiry, "tags", fieldTags, "status", fieldStatus, "spec", fieldSpec)

-- Set expiration.
local result = redis.pcall('EXPIRE', hashKey, ttl)
if type(result) == 'table' and result.err then
    redis.pcall('HDEL', hashKey, "ver", "id", "expiry", "tags", "status", "spec")
    return result.err
end

return ''
