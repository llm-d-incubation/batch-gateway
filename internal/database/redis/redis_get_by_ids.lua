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
local includeSpec = ARGV[2]

-- Check inputs.
local result = {}
if #keys == 0 then
	return {tonumber(0), result}
end

-- Pre-compute boolean to avoid string comparison in loop.
local shouldFilterSpec = (includeSpec == "false")
local needTenantFilter = (tenantID ~= nil and tenantID ~= '')

-- Iterate over the IDs.
for _, key in ipairs(keys) do
	-- Get the key's contents.
	local contents = redis.call('HGETALL', key)
	if #contents > 0 then
		-- Remove spec field if needed.
		if shouldFilterSpec then
			contents = remove_spec_field(contents)
		end
		-- Check tenant filter if needed.
		if needTenantFilter then
			local hash = contents_to_hash(contents)
			if tenantID == hash["tenantID"] then
				table.insert(result, contents)
			end
		else
			table.insert(result, contents)
		end
	end
end

-- Return the result.
return {tonumber(0), result}
