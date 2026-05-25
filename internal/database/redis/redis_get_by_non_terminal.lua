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

-- Get non-terminal batch items lua script.

-- Parse inputs.
local pattern = ARGV[1]
local start = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local tenantID = ARGV[4]
local includeSpec = ARGV[5]

-- Terminal statuses to exclude.
local terminal = {
	completed = true,
	failed = true,
	expired = true,
	cancelled = true,
}

-- Pre-compute boolean to avoid string comparison in loop.
local shouldFilterSpec = (includeSpec == "false")

-- Full scan: collect all matching non-terminal items.
local scan_cursor = "0"
local matched = {}
repeat
	local scan_out = redis.call('SCAN', scan_cursor, 'TYPE', 'hash', 'MATCH', pattern, 'COUNT', 100)
	scan_cursor = scan_out[1]
	for _, key in ipairs(scan_out[2]) do
		local contents = redis.call('HGETALL', key)
		local hash = contents_to_hash(contents)

		if tenantID == '' or hash["tenantID"] == tenantID then
			local skip = false
			local statusJSON = hash["status"]
			if statusJSON then
				local statusVal = string.match(statusJSON, '"status"%s*:%s*"([^"]+)"')
				if statusVal and terminal[statusVal] then
					skip = true
				end
			end
			if not skip then
				table.insert(matched, {key, contents})
			end
		end
	end
until scan_cursor == "0"

return paginate_results(matched, start, limit, shouldFilterSpec)
