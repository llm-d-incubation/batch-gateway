/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package regression

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

func loadGolden(t *testing.T, filename string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filename))
	if err != nil {
		t.Fatalf("failed to read golden file %s: %v", filename, err)
	}
	return data
}

func jsonKeys(t *testing.T, label string, data []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: failed to unmarshal as object: %v", label, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertJSONKeysMatch(t *testing.T, label string, golden, actual []byte) {
	t.Helper()

	goldenKeys := jsonKeys(t, label+" (golden)", golden)
	actualKeys := jsonKeys(t, label+" (actual)", actual)

	if !slices.Equal(goldenKeys, actualKeys) {
		missing, extra := diffKeys(goldenKeys, actualKeys)
		if len(missing) > 0 {
			t.Errorf("%s: missing keys (present in golden, absent in marshaled): %v", label, missing)
		}
		if len(extra) > 0 {
			t.Errorf("%s: extra keys (absent in golden, present in marshaled): %v", label, extra)
		}
	}

	var goldenMap, actualMap map[string]json.RawMessage
	// Errors already caught by jsonKeys above.
	_ = json.Unmarshal(golden, &goldenMap)
	_ = json.Unmarshal(actual, &actualMap)

	for key := range goldenMap {
		gVal := goldenMap[key]
		aVal, ok := actualMap[key]
		if !ok {
			continue
		}

		if isJSONObject(gVal) && isJSONObject(aVal) {
			assertJSONKeysMatch(t, label+"."+key, gVal, aVal)
		}

		if isJSONArrayOfObjects(gVal) && isJSONArrayOfObjects(aVal) {
			var gArr, aArr []json.RawMessage
			_ = json.Unmarshal(gVal, &gArr)
			_ = json.Unmarshal(aVal, &aArr)
			if len(gArr) > 0 && len(aArr) > 0 {
				assertJSONKeysMatch(t, label+"."+key+"[0]", gArr[0], aArr[0])
			}
		}
	}
}

func assertRoundTrip[T any](t *testing.T, goldenFile string) {
	t.Helper()

	golden := loadGolden(t, goldenFile)

	var obj T
	if err := json.Unmarshal(golden, &obj); err != nil {
		t.Fatalf("failed to unmarshal golden file into %T: %v", obj, err)
	}

	marshaled, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("failed to marshal %T back to JSON: %v", obj, err)
	}

	assertJSONKeysMatch(t, goldenFile, golden, marshaled)
	assertNullFieldsPreserved(t, goldenFile, golden, marshaled)
}

func assertNullFieldsPreserved(t *testing.T, label string, golden, actual []byte) {
	t.Helper()

	var goldenMap, actualMap map[string]json.RawMessage
	if err := json.Unmarshal(golden, &goldenMap); err != nil {
		return // Not a JSON object, skip
	}
	if err := json.Unmarshal(actual, &actualMap); err != nil {
		return // Not a JSON object, skip
	}

	for key, gVal := range goldenMap {
		aVal, ok := actualMap[key]
		if !ok {
			continue
		}

		if isNullValue(gVal) && !isNullValue(aVal) {
			t.Errorf("%s: field %q was null in golden, but not null after roundtrip", label, key)
		}

		if isJSONObject(gVal) && isJSONObject(aVal) {
			assertNullFieldsPreserved(t, label+"."+key, gVal, aVal)
		}

		if isJSONArrayOfObjects(gVal) && isJSONArrayOfObjects(aVal) {
			var gArr, aArr []json.RawMessage
			_ = json.Unmarshal(gVal, &gArr)
			_ = json.Unmarshal(aVal, &aArr)
			if len(gArr) > 0 && len(aArr) > 0 {
				assertNullFieldsPreserved(t, label+"."+key+"[0]", gArr[0], aArr[0])
			}
		}
	}
}

func diffKeys(golden, actual []string) (missing, extra []string) {
	goldenSet := make(map[string]bool, len(golden))
	for _, k := range golden {
		goldenSet[k] = true
	}
	actualSet := make(map[string]bool, len(actual))
	for _, k := range actual {
		actualSet[k] = true
	}

	for _, k := range golden {
		if !actualSet[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range actual {
		if !goldenSet[k] {
			extra = append(extra, k)
		}
	}
	return missing, extra
}

func isNullValue(data json.RawMessage) bool {
	return len(data) == 4 && string(data) == "null"
}

func isJSONObject(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}
	return data[0] == '{'
}

func isJSONArrayOfObjects(data json.RawMessage) bool {
	if len(data) < 2 {
		return false
	}
	if data[0] != '[' {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil || len(arr) == 0 {
		return false
	}
	return isJSONObject(arr[0])
}
