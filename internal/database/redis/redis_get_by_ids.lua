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

-- Get by IDs lua script.

-- Parse inputs.
local keys = KEYS
local tenantID = ARGV[1]

-- Check inputs.
local result = {}
if #keys == 0 then
	return {tonumber(0), result}
end

-- Iterate over the IDs.
for _, key in ipairs(keys) do
	-- Get the key's contents.
	local contents = redis.call('HGETALL', key)
	-- HGETALL returns a flat array: [field1, value1, field2, value2, ...]. Convert to a map.
	local hash = {}
	for i = 1, #contents, 2 do
		hash[contents[i]] = contents[i + 1]
	end
	-- Check inclusion condition.
	if (#contents > 0) and (tenantID == nil or tenantID == '' or tenantID == hash["tenantID"]) then
		table.insert(result, contents)
	end
end

-- Return the result.
return {tonumber(0), result}
