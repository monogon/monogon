// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package cni

// Opt doesn't do anything as all configuration is ignored.
type Opt func() error

func noopOpt() error {
	return nil
}

func WithConf(bytes []byte) Opt {
	return noopOpt
}

func WithConfFile(fileName string) Opt {
	return noopOpt
}

func WithConfIndex(bytes []byte, index int) Opt {
	return noopOpt
}

func WithConfListBytes(bytes []byte) Opt {
	return noopOpt
}

func WithConfListFile(fileName string) Opt {
	return noopOpt
}

func WithInterfacePrefix(prefix string) Opt {
	return noopOpt
}

func WithMinNetworkCount(count int) Opt {
	return noopOpt
}

func WithPluginConfDir(dir string) Opt {
	return noopOpt
}

func WithPluginDir(dirs []string) Opt {
	return noopOpt
}

func WithPluginMaxConfNum(max int) Opt {
	return noopOpt
}

func WithDefaultConf() error {
	return nil
}
func WithLoNetwork() error {
	return nil
}
