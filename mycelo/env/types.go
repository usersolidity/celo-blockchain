package env

import (
	"fmt"
	"math/big"
)

// Config represents mycelo environment parameters
type Config struct {
	ChainID  *big.Int       `json:"chainId"`  // chainId identifies the current chain and is used for replay protection
	Accounts AccountsConfig `json:"accounts"` // Accounts configuration for the environment
}

// AccountsConfig represents accounts configuration for the environment
type AccountsConfig struct {
	Mnemonic             string `json:"mnemonic"`           // Accounts mnemonic
	InitialValidators    int    `json:"initialValidators"`  // Number of initial validators
	ValidatorsPerGroup   int    `json:"validatorsPerGroup"` // Number of validators per group in the initial set
	DeveloperAccountsQty int    `json:"developerAccounts"`  // Number of developers accounts
}

// ValidatorGroup represents a group plus its validators members
type ValidatorGroup struct {
	Name       string
	Group      Account
	Validators []Account
}

// ValidatorGroupsQty retrieves the number of validator groups for the genesis
func (ac *AccountsConfig) ValidatorGroupsQty() int {
	if (ac.InitialValidators % ac.ValidatorsPerGroup) > 0 {
		return (ac.InitialValidators / ac.ValidatorsPerGroup) + 1
	}
	return ac.InitialValidators / ac.ValidatorsPerGroup
}

// AdminAccount returns the environment's admin account
func (ac *AccountsConfig) AdminAccount() *Account {
	acc, err := DeriveAccount(ac.Mnemonic, AdminAT, 0)
	if err != nil {
		panic(err)
	}
	return acc
}

// DeveloperAccounts returns the environment's developers accounts
func (ac *AccountsConfig) DeveloperAccounts() []Account {
	accounts, err := DeriveAccountList(ac.Mnemonic, DeveloperAT, ac.DeveloperAccountsQty)
	if err != nil {
		panic(err)
	}
	return accounts
}

// Account retrieves the account corresponding to the (accountType, idx)
func (ac *AccountsConfig) Account(accType AccountType, idx int) (*Account, error) {
	return DeriveAccount(ac.Mnemonic, accType, idx)
}

// ValidatorAccounts returns the environment's validators accounts
func (ac *AccountsConfig) ValidatorAccounts() []Account {
	accounts, err := DeriveAccountList(ac.Mnemonic, ValidatorAT, ac.InitialValidators)
	if err != nil {
		panic(err)
	}
	return accounts
}

// ValidatorGroupAccounts returns the environment's validators group accounts
func (ac *AccountsConfig) ValidatorGroupAccounts() []Account {
	accounts, err := DeriveAccountList(ac.Mnemonic, ValidatorGroupAT, ac.ValidatorGroupsQty())
	if err != nil {
		panic(err)
	}
	return accounts
}

// ValidatorGroups return the list of validator groups on genesis
func (ac *AccountsConfig) ValidatorGroups() []ValidatorGroup {
	groups := make([]ValidatorGroup, ac.ValidatorGroupsQty())

	groupAccounts := ac.ValidatorGroupAccounts()
	validatorAccounts := ac.ValidatorAccounts()

	for i := 0; i < (len(groups) - 1); i++ {
		groups[i] = ValidatorGroup{
			Name:       fmt.Sprintf("group %02d", i+1),
			Group:      groupAccounts[i],
			Validators: validatorAccounts[ac.ValidatorsPerGroup*i : ac.ValidatorsPerGroup*(i+1)],
		}
	}

	// last group might not be full, use an open slice for validators
	i := len(groups) - 1
	groups[i] = ValidatorGroup{
		Name:       fmt.Sprintf("group %02d", i+1),
		Group:      groupAccounts[i],
		Validators: validatorAccounts[ac.ValidatorsPerGroup*i:],
	}

	return groups
}