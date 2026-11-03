package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/travisjeffery/radar"
	"go.yaml.in/yaml/v4"
)

// loadReviewCriteria reads the rules file the policy names and the guidance
// files from the trusted checkout holding the policy, and the packs the rules
// name from packsDir.
func loadReviewCriteria(policyPath string, policy radar.PullRequestPolicy, packsDir string) (radar.ReviewCriteria, error) {
	var c radar.ReviewCriteria
	rulesPath, err := checkoutFile(policyPath, policy.ReviewCriteria.Rules)
	if err != nil {
		return c, fmt.Errorf("locating review_criteria.rules: %w", err)
	}
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		return c, fmt.Errorf("reading review_criteria.rules: %w", err)
	}
	rules, err := parseReviewRules(data)
	if err != nil {
		return c, fmt.Errorf("%s: %w", policy.ReviewCriteria.Rules, err)
	}
	sum := sha256.Sum256(data)
	c = radar.ReviewCriteria{
		RulesPath: policy.ReviewCriteria.Rules, RulesSHA256: hex.EncodeToString(sum[:]),
		Rules: rules, Packs: map[string]radar.ReviewPack{}, Guidance: map[string][]byte{},
	}

	ids := rules.PackIDs()
	if len(ids) > 0 && packsDir == "" {
		return c, errors.New("the review rules name packs; pass -review-packs")
	}
	for _, id := range ids {
		data, err := os.ReadFile(filepath.Join(packsDir, id+".md"))
		if err != nil {
			return c, fmt.Errorf("reading review pack %s: %w", id, err)
		}
		pack, err := radar.ParseReviewPack(id, data)
		if err != nil {
			return c, err
		}
		c.Packs[id] = pack
	}

	root := strings.TrimSuffix(rulesPath, filepath.FromSlash(policy.ReviewCriteria.Rules))
	files, err := trackedFiles(root)
	if err != nil {
		return c, err
	}
	for _, p := range files {
		if !rules.IsGuidanceFile(p) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return c, fmt.Errorf("reading guidance %s: %w", p, err)
		}
		c.Guidance[p] = data
	}
	return c, c.Validate()
}

// parseReviewRules decodes exactly one YAML document, rejecting unknown keys
// so a misspelt policy knob cannot silently do nothing.
func parseReviewRules(data []byte) (radar.ReviewRules, error) {
	var rules radar.ReviewRules
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&rules); err != nil {
		return rules, fmt.Errorf("decoding review rules: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return rules, errors.New("review rules must be one YAML document")
	}
	return rules, nil
}

// trackedFiles lists the files git tracks in the checkout, so guidance is
// read only from committed default-branch content.
func trackedFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("listing tracked files: %w", err)
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}
