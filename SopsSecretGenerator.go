// Copyright 2019-2020 Go About B.V. and contributors
// Copyright 2024-2025 Freightdog B.V. and contributors
// Parts adapted from kustomize, Copyright 2019 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0.

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleContainerTools/kpt-functions-sdk/go/fn"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/decrypt"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

const apiVersion = "kustomize.freightdog.com/v1"
const kind = "SopsSecretGenerator"

var utf8bom = []byte{0xEF, 0xBB, 0xBF}
var stripAnnotations = map[string]bool{
	"config.kubernetes.io/local-config": true,
	"config.kubernetes.io/function":     true,
}

type kvMap map[string]string

// TypeMeta defines the resource type
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

// ObjectMeta contains Kubernetes resource metadata such as the name
type ObjectMeta struct {
	Name        string `json:"name" yaml:"name"`
	Namespace   string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Labels      kvMap  `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations kvMap  `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// SopsSecretGenerator is a generator for Secrets
type SopsSecretGenerator struct {
	TypeMeta              `json:",inline" yaml:",inline"`
	ObjectMeta            `json:"metadata" yaml:"metadata"`
	EnvSources            []string `json:"envs" yaml:"envs"`
	FileSources           []string `json:"files" yaml:"files"`
	Behavior              string   `json:"behavior,omitempty" yaml:"behavior,omitempty"`
	DisableNameSuffixHash bool     `json:"disableNameSuffixHash,omitempty" yaml:"disableNameSuffixHash,omitempty"`
	Type                  string   `json:"type,omitempty" yaml:"type,omitempty"`
}

// Secret is a Kubernetes Secret
type Secret struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Data       kvMap  `json:"data" yaml:"data"`
	Type       string `json:"type,omitempty" yaml:"type,omitempty"`
}

func usage() {
	usage := `
		SopsSecretGenerator is a Kustomize generator plugin that generates Secrets from sops-encrypted files.

		Note:
		  The usage examples here are for standalone execution. If the plugin is used via Kustomize,
		  then Kustomize will handle passing data to the plugin.

		Usage:
		  cat ResourceList.yaml | SopsSecretGenerator
`

	_, _ = fmt.Fprintf(os.Stderr, "%s", strings.ReplaceAll(usage, "		", ""))
	os.Exit(1)
}

func main() {
	stdinStat, _ := os.Stdin.Stat()

	// Check the StdIn content.
	if (stdinStat.Mode() & os.ModeCharDevice) != 0 {
		usage()
	}

	err := fn.AsMain(fn.ResourceListProcessorFunc(generateKRMManifest))
	if err != nil {
		fmt.Println(err)
		usage()
	}
}

// generateKRMManifest reads ResourceList with SopsSecretGenerator items
// and returns ResourceList with Secret items.
func generateKRMManifest(rl *fn.ResourceList) (bool, error) {
	var generatedSecrets fn.KubeObjects

	for _, sopsSecretGeneratorManifest := range rl.Items {
		secretManifest, err := processSopsSecretGenerator([]byte(sopsSecretGeneratorManifest.String()))
		if err != nil {
			rl.LogResult(err)
			return false, err
		}

		secretKubeObject, err := fn.ParseKubeObject([]byte(secretManifest))
		if err != nil {
			rl.LogResult(err)
			return false, err
		}

		generatedSecrets = append(generatedSecrets, secretKubeObject)
	}

	rl.Items = generatedSecrets

	return true, nil
}

func processSopsSecretGenerator(manifestContent []byte) (string, error) {
	input, err := readInput(manifestContent)
	if err != nil {
		return "", err
	}
	secret, err := generateSecret(input)
	if err != nil {
		return "", err
	}
	output, err := yaml.Marshal(secret)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func generateSecret(sopsSecret SopsSecretGenerator) (Secret, error) {
	data, err := parseInput(sopsSecret)
	if err != nil {
		return Secret{}, err
	}

	annotations := make(kvMap)
	for k, v := range sopsSecret.Annotations {
		if _, skip := stripAnnotations[k]; skip {
			continue
		}
		annotations[k] = v
	}
	if !sopsSecret.DisableNameSuffixHash {
		annotations["kustomize.config.k8s.io/needs-hash"] = "true"
	}
	if sopsSecret.Behavior != "" {
		annotations["kustomize.config.k8s.io/behavior"] = sopsSecret.Behavior
	}

	secret := Secret{
		TypeMeta: TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: ObjectMeta{
			Name:        sopsSecret.Name,
			Namespace:   sopsSecret.Namespace,
			Labels:      sopsSecret.Labels,
			Annotations: annotations,
		},
		Data: data,
		Type: sopsSecret.Type,
	}
	return secret, nil
}

func readFile(fileName string) ([]byte, error) {
	content, err := os.ReadFile(fileName)
	if err != nil {
		return []byte{}, err
	}

	return content, nil
}

func readInput(manifestContent []byte) (SopsSecretGenerator, error) {
	input := SopsSecretGenerator{
		TypeMeta: TypeMeta{},
		ObjectMeta: ObjectMeta{
			Annotations: make(kvMap),
		},
	}

	err := yaml.Unmarshal(manifestContent, &input)
	if err != nil {
		return SopsSecretGenerator{}, err
	}

	if input.APIVersion != apiVersion || input.Kind != kind {
		return SopsSecretGenerator{}, errors.Errorf("input must be apiVersion %s, kind %s", apiVersion, kind)
	}
	if input.Name == "" {
		return SopsSecretGenerator{}, errors.New("input must contain metadata.name value")
	}
	return input, nil
}

func parseInput(input SopsSecretGenerator) (kvMap, error) {
	data := make(kvMap)
	err := parseEnvSources(input.EnvSources, data)
	if err != nil {
		return nil, err
	}
	err = parseFileSources(input.FileSources, data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func parseEnvSources(sources []string, data kvMap) error {
	for _, source := range sources {
		err := parseEnvSource(source, data)
		if err != nil {
			return errors.Wrapf(err, "env source \"%s\"", source)
		}
	}
	return nil
}

func parseEnvSource(source string, data kvMap) error {
	decrypted, err := decryptFile(source)
	if err != nil {
		return err
	}

	switch formats.FormatForPath(source) {
	case formats.Dotenv:
		err = parseDotEnvContent(decrypted, data)
	case formats.Yaml:
		err = parseYAMLContent(decrypted, data)
	case formats.Json:
		err = parseJSONContent(decrypted, data)
	default:
		err = errors.New("unknown file format, use dotenv, yaml or json")
	}
	if err != nil {
		return err
	}

	return nil
}

func parseDotEnvContent(content []byte, data kvMap) error {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		// Strip UTF-8 byte order mark from first line
		if lineNum == 0 {
			line = bytes.TrimPrefix(line, utf8bom)
		}
		err := parseDotEnvLine(line, data)
		if err != nil {
			return errors.Wrapf(err, "line %d", lineNum)
		}
		lineNum++
	}
	return scanner.Err()
}

func parseDotEnvLine(line []byte, data kvMap) error {
	if !utf8.Valid(line) {
		return fmt.Errorf("invalid UTF-8 bytes: %v", string(line))
	}

	line = bytes.TrimLeftFunc(line, unicode.IsSpace)

	if len(line) == 0 || line[0] == '#' {
		return nil
	}

	pair := strings.SplitN(string(line), "=", 2)
	if len(pair) != 2 {
		return fmt.Errorf("requires value: %v", string(line))
	}

	data[pair[0]] = base64.StdEncoding.EncodeToString([]byte(pair[1]))
	return nil
}

func parseYAMLContent(content []byte, data kvMap) error {
	d := make(kvMap)
	err := yaml.Unmarshal(content, &d)
	if err != nil {
		return err
	}
	for k, v := range d {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return nil
}

func parseJSONContent(content []byte, data kvMap) error {
	d := make(kvMap)
	err := json.Unmarshal(content, &d)
	if err != nil {
		return err
	}
	for k, v := range d {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return nil
}

func parseFileSources(sources []string, data kvMap) error {
	for _, source := range sources {
		err := parseFileSource(source, data)
		if err != nil {
			return errors.Wrapf(err, "file source \"%s\"", source)
		}
	}
	return nil
}

func decryptFile(source string) ([]byte, error) {
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, errors.Wrap(err, "could not read file")
	}

	decrypted, err := decrypt.DataWithFormat(content, formats.FormatForPath(source))
	if err != nil {
		return nil, errors.Wrap(err, "sops could not decrypt")
	}
	return decrypted, nil
}

func parseFileSource(source string, data kvMap) error {
	key, fname, err := parseFileName(source)
	if err != nil {
		return err
	}

	decrypted, err := decryptFile(fname)
	if err != nil {
		return err
	}

	data[key] = base64.StdEncoding.EncodeToString(decrypted)
	return nil
}

func parseFileName(source string) (key string, fn string, err error) {
	components := strings.Split(source, "=")

	switch len(components) {
	case 1:
		return path.Base(source), source, nil
	case 2:
		key, fn = components[0], components[1]
		if key == "" {
			return "", "", fmt.Errorf("key name for file path \"%s\" missing", fn)
		} else if fn == "" {
			return "", "", fmt.Errorf("file path for key name \"%s\" missing", key)
		}
		return key, fn, nil
	default:
		return "", "", errors.New("key names or file paths cannot contain '='")
	}
}
