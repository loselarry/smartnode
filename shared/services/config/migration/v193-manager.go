package migration

import (
	"fmt"

	"github.com/rocket-pool/smartnode/shared/types/config"
)

func upgradeFromV193(serializedConfig map[string]map[string]string) error {
	// v1.9.3 had the BN API port mode as a boolean
	if err := updateRPCPortConfig(serializedConfig, "consensusCommon", "openApiPort"); err != nil {
		return err
	}
	if err := updateRPCPortConfig(serializedConfig, "prysm", "openRpcPort"); err != nil {
		return err
	}
	if err := updateRPCPortConfig(serializedConfig, "executionCommon", "openRpcPorts"); err != nil {
		return err
	}
	if err := updateRPCPortConfig(serializedConfig, "mevBoost", "openRpcPort"); err != nil {
		return err
	}
	if err := updateRPCPortConfig(serializedConfig, "prometheus", "openPort"); err != nil {
		return err
	}
	return nil
}

func updateRPCPortConfig(serializedConfig map[string]map[string]string, configKeyString string, keyOpenPorts string) error {
	// v1.9.3 had the API ports mode as a boolean
	configSection, exists := serializedConfig[configKeyString]
	if !exists {
		return fmt.Errorf("expected a section called `%s` but it didn't exist", configKeyString)
	}
	openRPCPorts, exists := configSection[keyOpenPorts]
	if !exists {
		return fmt.Errorf("expected a executionCommon setting named `%s` but it didn't exist", keyOpenPorts)
	}

	// Update the config
	if openRPCPorts == "true" {
		configSection[keyOpenPorts] = config.RPC_OpenLocalhost.String()
	} else {
		configSection[keyOpenPorts] = config.RPC_Closed.String()
	}
	return nil
}
