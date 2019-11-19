// VulcanizeDB
// Copyright © 2019 Vulcanize

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.

// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package transformer

import (
	"errors"

	"github.com/sirupsen/logrus"

	"github.com/makerdao/vulcanizedb/pkg/config"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/full/converter"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/full/retriever"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/shared/contract"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/shared/parser"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/shared/poller"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/shared/repository"
	"github.com/makerdao/vulcanizedb/pkg/contract_watcher/shared/types"
	"github.com/makerdao/vulcanizedb/pkg/core"
	"github.com/makerdao/vulcanizedb/pkg/datastore"
	"github.com/makerdao/vulcanizedb/pkg/datastore/postgres"
	"github.com/makerdao/vulcanizedb/pkg/datastore/postgres/repositories"
)

// Transformer is the top level struct for transforming watched contract data
// Requires a fully synced vDB and a running eth node (or infura)
type Transformer struct {
	// Database interfaces
	FilterRepository           datastore.FilterRepository       // Log filters repo; accepts filters generated by Contract.GenerateFilters()
	WatchedEventRepository     datastore.WatchedEventRepository // Watched event log views, created by the log filters
	TransformedEventRepository repository.EventRepository       // Holds transformed watched event log data

	// Pre-processing interfaces
	Parser    parser.Parser            // Parses events and methods out of contract abi fetched using contract address
	Retriever retriever.BlockRetriever // Retrieves first block for contract and current block height

	// Processing interfaces
	Converter converter.ConverterInterface // Converts watched event logs into custom log
	Poller    poller.Poller                // Polls methods using contract's token holder addresses and persists them using method datastore

	// Store contract configuration information
	Config config.ContractConfig

	// Store contract info as mapping to contract address
	Contracts map[string]*contract.Contract

	// Latest block in the block repository
	LastBlock int64
}

// NewTransformer takes in contract config, blockchain, and database, and returns a new Transformer
func NewTransformer(con config.ContractConfig, BC core.BlockChain, DB *postgres.DB) *Transformer {
	return &Transformer{
		Poller:                     poller.NewPoller(BC, DB, types.FullSync),
		Parser:                     parser.NewParser(con.Network),
		Retriever:                  retriever.NewBlockRetriever(DB),
		Converter:                  &converter.Converter{},
		Contracts:                  map[string]*contract.Contract{},
		WatchedEventRepository:     repositories.WatchedEventRepository{DB: DB},
		FilterRepository:           repositories.FilterRepository{DB: DB},
		TransformedEventRepository: repository.NewEventRepository(DB, types.FullSync),
		Config:                     con,
	}
}

// Init initializes the transformer
// Use after creating and setting transformer
// Loops over all of the addr => filter sets
// Uses parser to pull event info from abi
// Use this info to generate event filters
func (tr *Transformer) Init() error {
	for contractAddr := range tr.Config.Addresses {
		// Configure Abi
		if tr.Config.Abis[contractAddr] == "" {
			// If no abi is given in the config, this method will try fetching from internal look-up table and etherscan
			err := tr.Parser.Parse(contractAddr)
			if err != nil {
				return err
			}
		} else {
			// If we have an abi from the config, load that into the parser
			err := tr.Parser.ParseAbiStr(tr.Config.Abis[contractAddr])
			if err != nil {
				return err
			}
		}

		// Get first block and most recent block number in the header repo
		firstBlock, err := tr.Retriever.RetrieveFirstBlock(contractAddr)
		if err != nil {
			return err
		}
		// Set to specified range if it falls within the bounds
		if firstBlock < tr.Config.StartingBlocks[contractAddr] {
			firstBlock = tr.Config.StartingBlocks[contractAddr]
		}

		// Get contract name if it has one
		var name = new(string)
		pollingErr := tr.Poller.FetchContractData(tr.Parser.Abi(), contractAddr, "name", nil, name, tr.LastBlock)
		if pollingErr != nil {
			// can't return this error because "name" might not exist on the contract
			logrus.Warnf("error fetching contract data: %s", pollingErr.Error())
		}

		// Remove any potential accidental duplicate inputs in arg filter values
		eventArgs := map[string]bool{}
		for _, arg := range tr.Config.EventArgs[contractAddr] {
			eventArgs[arg] = true
		}
		methodArgs := map[string]bool{}
		for _, arg := range tr.Config.MethodArgs[contractAddr] {
			methodArgs[arg] = true
		}

		// Aggregate info into contract object
		info := contract.Contract{
			Name:          *name,
			Network:       tr.Config.Network,
			Address:       contractAddr,
			Abi:           tr.Parser.Abi(),
			ParsedAbi:     tr.Parser.ParsedAbi(),
			StartingBlock: firstBlock,
			Events:        tr.Parser.GetEvents(tr.Config.Events[contractAddr]),
			Methods:       tr.Parser.GetSelectMethods(tr.Config.Methods[contractAddr]),
			FilterArgs:    eventArgs,
			MethodArgs:    methodArgs,
			Piping:        tr.Config.Piping[contractAddr],
		}.Init()

		// Use info to create filters
		err = info.GenerateFilters()
		if err != nil {
			return err
		}

		// Iterate over filters and push them to the repo using filter repository interface
		for _, filter := range info.Filters {
			err = tr.FilterRepository.CreateFilter(filter)
			if err != nil {
				return err
			}
		}

		// Store contract info for further processing
		tr.Contracts[contractAddr] = info
	}

	// Get the most recent block number in the block repo
	var err error
	tr.LastBlock, err = tr.Retriever.RetrieveMostRecentBlock()
	if err != nil {
		return err
	}

	return nil
}

// Execute runs the transformation processes
// Iterates through stored, initialized contract objects
// Iterates through contract's event filters, grabbing watched event logs
// Uses converter to convert logs into custom log type
// Persists converted logs into custom postgres tables
// Calls selected methods, using token holder address generated during event log conversion
func (tr *Transformer) Execute() error {
	if len(tr.Contracts) == 0 {
		return errors.New("error: transformer has no initialized contracts to work with")
	}
	// Iterate through all internal contracts
	for _, con := range tr.Contracts {
		// Update converter with current contract
		tr.Converter.Update(con)

		// Iterate through contract filters and get watched event logs
		for eventSig, filter := range con.Filters {
			watchedEvents, err := tr.WatchedEventRepository.GetWatchedEvents(filter.Name)
			if err != nil {
				return err
			}

			// Iterate over watched event logs
			for _, we := range watchedEvents {
				// Convert them to our custom log type
				cstm, err := tr.Converter.Convert(*we, con.Events[eventSig])
				if err != nil {
					return err
				}
				if cstm == nil {
					continue
				}

				// If log is not empty, immediately persist in repo
				// Run this in seperate goroutine?
				err = tr.TransformedEventRepository.PersistLogs([]types.Log{*cstm}, con.Events[eventSig], con.Address, con.Name)
				if err != nil {
					return err
				}
			}
		}

		// After persisting all watched event logs
		// poller polls select contract methods
		// and persists the results into custom pg tables
		if err := tr.Poller.PollContract(*con, tr.LastBlock); err != nil {
			return err
		}
	}

	// At the end of a transformation cycle, and before the next
	// update the latest block from the block repo
	var err error
	tr.LastBlock, err = tr.Retriever.RetrieveMostRecentBlock()
	if err != nil {
		return err
	}

	return nil
}

// GetConfig returns the transformers config; satisfies the transformer interface
func (tr *Transformer) GetConfig() config.ContractConfig {
	return tr.Config
}
