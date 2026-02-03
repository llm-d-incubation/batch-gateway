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
local getStatic = ARGV[1]
local pattern = ARGV[2]
local tagsCond = ARGV[3]
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
		contents = redis.call('HMGET', key, "id", "expiry", "tags", "status", "spec")
	else
		contents = redis.call('HMGET', key, "id", "expiry", "tags", "status")
	end
	-- Search for the tags.
	local ofound = 0
	if (#tags > 0) and (tagsCond == 'and' or tagsCond == 'or') then
		for _, tag in ipairs(tags) do
			local found = string.find(contents[3], tag, 0, true)
			if found ~= nil then
				ofound = ofound + 1
			end
		end
	end
	-- Add the key entry to the result table, if the keys satisfies the condition.
	if (tagsCond == 'na') or (#tags == 0) or (tagsCond == 'and' and ofound == #tags) or (tagsCond == 'or' and ofound > 0) then
		table.insert(result, contents)
	end
end

-- Return the result.
return {tonumber(scan_out[1]), result}
