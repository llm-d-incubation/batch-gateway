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

-- Get by tenant lua script.

-- Parse inputs.
local tenantID = ARGV[1]
local pattern = ARGV[2]
local cursor = ARGV[3]
local count = ARGV[4]
local includeSpec = ARGV[5]

-- Check inputs.
local result = {}
if tenantID == nil or tenantID == '' then
	return {0, result}
end

-- Pre-compute boolean to avoid string comparison in loop.
local shouldFilterSpec = (includeSpec == "false")

-- Get the keys for the current iteration.
local scan_out = redis.call('SCAN', cursor, 'TYPE', 'hash', 'MATCH', pattern, 'COUNT', count)

-- Iterate over the keys.
for _, key in ipairs(scan_out[2]) do
	-- Get the key's contents.
	local contents = redis.call('HGETALL', key)
	-- Create a map of the contents.
	local hash = contents_to_hash(contents)
	-- Check inclusion condition.
	if tenantID == hash["tenantID"] then
		-- Remove spec field if needed.
		if shouldFilterSpec then
			contents = remove_spec_field(contents)
		end
		-- Add the item to the result.
		table.insert(result, contents)
	end
end

-- Return the result.
return {tonumber(scan_out[1]), result}
