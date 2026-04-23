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

-- Common utility functions for Redis Lua scripts.

-- Convert a flat HGETALL result array to a hash map.
-- HGETALL returns a flat array: [field1, value1, field2, value2, ...]
-- Returns a map/table: {field1 = value1, field2 = value2, ...}
local function contents_to_hash(contents)
	local hash = {}
	for i = 1, #contents, 2 do
		hash[contents[i]] = contents[i + 1]
	end
	return hash
end

-- Remove the "spec" field and its value from a contents array.
-- contents is a flat array: [field1, value1, field2, value2, ...]
-- Returns a new filtered array without the "spec" field.
local function remove_spec_field(contents)
	local filtered = {}
	for i = 1, #contents, 2 do
		if contents[i] ~= "spec" then
			table.insert(filtered, contents[i])
			table.insert(filtered, contents[i + 1])
		end
	end
	return filtered
end

-- Sort matched items by key, slice the requested page, optionally filter spec,
-- and return {next_cursor, sliced}. next_cursor is 0 when there are no more items.
-- matched is a list of {key, contents} pairs.
local function paginate_results(matched, start, limit, shouldFilterSpec)
	table.sort(matched, function(a, b) return a[1] < b[1] end)
	local sliced = {}
	for i = start + 1, math.min(start + limit, #matched) do
		local contents = matched[i][2]
		if shouldFilterSpec then
			contents = remove_spec_field(contents)
		end
		table.insert(sliced, contents)
	end
	local next_cursor = start + #sliced
	if next_cursor >= #matched then
		next_cursor = 0
	end
	return {next_cursor, sliced}
end
