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

// This file provides common utilities.

package com

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
)

var (
	letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
)

func RandString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

// GetFolderNameByTenantID converts a tenant ID into a filesystem and S3-safe folder name.
// It generates a deterministic SHA256-based name that is guaranteed to be valid for both
// filesystem paths and S3 bucket naming requirements (63 characters max).
func GetFolderNameByTenantID(tenantID string) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("tenantID cannot be empty")
	}

	// Generate SHA256 hash of the tenant ID for a deterministic, filesystem-safe name
	hash := sha256.Sum256([]byte(tenantID))
	hashStr := hex.EncodeToString(hash[:])

	// Use "t-" prefix + 61 hex chars = 63 chars total (S3 maximum)
	// This provides virtually collision-free tenant ID mapping while staying under S3's 63-char limit
	return "t-" + hashStr[:61], nil
}
