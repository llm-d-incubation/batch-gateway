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

-- Get by tags lua script.

-- Parse inputs.
local tags = KEYS
local tagsCond = ARGV[1]
local includeStatic = ARGV[2]
local pattern = ARGV[3]
local cursor = ARGV[4]
local count = ARGV[5]
local tenantID = ARGV[6]

-- Check inputs.
local result = {}
if (tags == nil) or (#tags == 0) or (tagsCond ~= 'and' and tagsCond ~= 'or') then
	return {0, result}
end

-- Get the keys for the current iteration.
local scan_out = redis.call('SCAN', cursor, 'TYPE', 'hash', 'MATCH', pattern, 'COUNT', count)

-- Iterate over the keys.
for _, key in ipairs(scan_out[2]) do
	-- Get the key's contents.
	local contents
	if includeStatic == 'true' then
		contents = redis.call('HMGET', key, "ID", "tenantID", "expiry", "tags", "purpose", "status", "spec")
	else
		contents = redis.call('HMGET', key, "ID", "tenantID", "expiry", "tags", "purpose", "status")
	end
	-- Search for the tags.
	local ofound = 0
	if (#tags > 0) and (tagsCond == 'and' or tagsCond == 'or') then
		for _, tag in ipairs(tags) do
			local found = string.find(contents[4], tag, 0, true)
			if found ~= nil then
				ofound = ofound + 1
			end
		end
	end
	-- Check inclusion condition.
	if ((tagsCond == 'and' and ofound == #tags) or (tagsCond == 'or' and ofound > 0)) and (tenantID == nil or tenantID == '' or tenantID == contents[2]) then
		table.insert(result, contents)
	end
end

-- Return the result.
return {tonumber(scan_out[1]), result}
