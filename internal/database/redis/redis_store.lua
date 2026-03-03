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
local fieldItemType = ARGV[1]
local fieldVer = ARGV[2]
local fieldID = ARGV[3]
local fieldTenantID = ARGV[4]
local fieldExpiry = tonumber(ARGV[5])
local fieldTags = ARGV[6]
local fieldStatus = ARGV[7]
local fieldSpec = ARGV[8]
local ttl = tonumber(ARGV[9])
local fieldPurpose = ARGV[10]

if fieldItemType == 'batch' then
    -- Add the hash key.
    redis.call('HSET', hashKey, "ver", fieldVer,
        "ID", fieldID, "tenantID", fieldTenantID, "expiry", fieldExpiry, "tags", fieldTags,
        "status", fieldStatus, "spec", fieldSpec)

    -- Set expiration.
    local result = redis.pcall('EXPIRE', hashKey, ttl)
    if type(result) == 'table' and result.err then
        redis.pcall('DEL', hashKey)
        return result.err
    end
else
    -- Add the hash key.
    redis.call('HSET', hashKey, "ver", fieldVer,
        "ID", fieldID, "tenantID", fieldTenantID, "expiry", fieldExpiry, "tags", fieldTags,
        "status", fieldStatus, "spec", fieldSpec, "purpose", fieldPurpose)

    -- Set expiration.
    local result = redis.pcall('EXPIRE', hashKey, ttl)
    if type(result) == 'table' and result.err then
        redis.pcall('DEL', hashKey)
        return result.err
    end
end

return ''
