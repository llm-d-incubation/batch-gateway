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

-- Get by expiry lua script.

-- Parse inputs.
local expTime = tonumber(ARGV[1])
local pattern = ARGV[2]
local getStatic = ARGV[3]
local cursor = ARGV[4]
local count = ARGV[5]

-- Get the keys for the current iteration.
local scan_out = redis.call('SCAN', cursor, 'TYPE', 'hash', 'MATCH', pattern, 'COUNT', count)

-- Iterate over the keys.
local result = {}
for _, key in ipairs(scan_out[2]) do
	-- Get the key's contents.
	local contents
	if getStatic == 'true' then
		contents = redis.call('HMGET', key, "id", "tenantID", "expiry", "tags", "status", "spec")
	else
		contents = redis.call('HMGET', key, "id", "tenantID", "expiry", "tags", "status")
	end
	-- Check for expiry condition.
	if tonumber(contents[3]) <= expTime then
		table.insert(result, contents)
	end
end

-- Return the result.
return {tonumber(scan_out[1]), result}
