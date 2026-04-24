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
local start = tonumber(ARGV[3])
local limit = tonumber(ARGV[4])
local includeSpec = ARGV[5]

-- Check inputs.
local result = {}
if tenantID == nil or tenantID == '' then
	return {0, result}
end

-- Pre-compute boolean to avoid string comparison in loop.
local shouldFilterSpec = (includeSpec == "false")

-- Full scan: collect all matching items.
local scan_cursor = "0"
local matched = {}
repeat
	local scan_out = redis.call('SCAN', scan_cursor, 'TYPE', 'hash', 'MATCH', pattern, 'COUNT', 100)
	scan_cursor = scan_out[1]
	for _, key in ipairs(scan_out[2]) do
		local contents = redis.call('HGETALL', key)
		local hash = contents_to_hash(contents)
		if tenantID == hash["tenantID"] then
			table.insert(matched, {key, contents})
		end
	end
until scan_cursor == "0"

return paginate_results(matched, start, limit, shouldFilterSpec)
