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
local IDs = KEYS
local includeStatic = ARGV[1]
local tenantId = ARGV[2]

-- Check inputs.
local result = {}
if #IDs == 0 then
	return {tonumber(0), result}
end

-- Iterate over the IDs.
for _, key in ipairs(IDs) do
	-- Get the key's contents.
	local contents
	if includeStatic == 'true' then
		contents = redis.call('HMGET', key, "id", "tenantId", "expiry", "tags", "status", "spec")
	else
		contents = redis.call('HMGET', key, "id", "tenantId", "expiry", "tags", "status")
	end
	-- Check inclusion condition.
	if (contents ~= nil) and (tenantId == '' or tenantId == contents[2]) then
		table.insert(result, contents)
	end
end

-- Return the result.
return {tonumber(0), result}
