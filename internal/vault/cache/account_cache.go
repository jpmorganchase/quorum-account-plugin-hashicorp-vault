// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package cache

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/goquorum/quorum-plugin-hashicorp-account-store/internal/vault"
)

// Minimum amount of time between cache reloads. This limit applies if the platform does
// not support change notifications. It also applies if the keystore directory does not
// exist yet, the code will attempt to create a watcher at most this often.
const minReloadInterval = 2 * time.Second

//type accountsByURL []accounts.Account
//
//func (s accountsByURL) Len() int           { return len(s) }
//func (s accountsByURL) Less(i, j int) bool { return s[i].URL.Cmp(s[j].URL) < 0 }
//func (s accountsByURL) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// AmbiguousAddrError is returned when attempting to unlock
// an address for which more than one file exists.
type AmbiguousAddrError struct {
	Addr    common.Address
	Matches []accounts.Account
}

func (err *AmbiguousAddrError) Error() string {
	files := ""
	for i, a := range err.Matches {
		files += a.URL.Path
		if i < len(err.Matches)-1 {
			files += ", "
		}
	}
	return fmt.Sprintf("multiple keys match address (%s)", files)
}

// accountCache is a live index of all accounts in the keystore.
type AccountCache struct {
	keydir   string
	watcher  *watcher
	Mu       sync.Mutex
	All      vault.AccountsByURL
	ByAddr   map[common.Address][]accounts.Account
	throttle *time.Timer
	notify   chan struct{}
	fileC    fileCache

	accountConfigUnmarshaller vault.AccountConfigUnmarshaller
	unlocker                  vault.Unlocker
	toUnlock                  []common.Address
}

func NewAccountCache(keydir string, unlocker vault.Unlocker, toUnlock []common.Address, unmarshaller vault.AccountConfigUnmarshaller) (*AccountCache, chan struct{}) {
	ac := &AccountCache{
		keydir:                    keydir,
		ByAddr:                    make(map[common.Address][]accounts.Account),
		notify:                    make(chan struct{}, 1),
		fileC:                     fileCache{all: mapset.NewThreadUnsafeSet()},
		unlocker:                  unlocker,
		toUnlock:                  toUnlock,
		accountConfigUnmarshaller: unmarshaller,
	}
	ac.watcher = newWatcher(ac)
	return ac, ac.notify
}

func (ac *AccountCache) Accounts() []accounts.Account {
	ac.MaybeReload()
	ac.Mu.Lock()
	defer ac.Mu.Unlock()
	cpy := make([]accounts.Account, len(ac.All))
	copy(cpy, ac.All)
	return cpy
}

func (ac *AccountCache) HasAddress(addr common.Address) bool {
	ac.MaybeReload()
	ac.Mu.Lock()
	defer ac.Mu.Unlock()
	return len(ac.ByAddr[addr]) > 0
}

func (ac *AccountCache) Add(newAccount accounts.Account) {
	ac.Mu.Lock()
	defer ac.Mu.Unlock()

	i := sort.Search(len(ac.All), func(i int) bool { return ac.All[i].URL.Cmp(newAccount.URL) >= 0 })
	if i < len(ac.All) && ac.All[i] == newAccount {
		return
	}
	// newAccount is not in the cache.
	ac.All = append(ac.All, accounts.Account{})
	copy(ac.All[i+1:], ac.All[i:])
	ac.All[i] = newAccount
	ac.ByAddr[newAccount.Address] = append(ac.ByAddr[newAccount.Address], newAccount)
}

// note: removed needs to be unique here (i.e. both File and Address must be set).
func (ac *AccountCache) delete(removed accounts.Account) {
	ac.Mu.Lock()
	defer ac.Mu.Unlock()

	ac.All = removeAccount(ac.All, removed)
	if ba := removeAccount(ac.ByAddr[removed.Address], removed); len(ba) == 0 {
		delete(ac.ByAddr, removed.Address)
	} else {
		ac.ByAddr[removed.Address] = ba
	}
}

// deleteByFile removes an account referenced by the given path.
func (ac *AccountCache) deleteByFile(path string) {
	ac.Mu.Lock()
	defer ac.Mu.Unlock()
	i := sort.Search(len(ac.All), func(i int) bool { return ac.All[i].URL.Path >= path })

	if i < len(ac.All) && ac.All[i].URL.Path == path {
		removed := ac.All[i]
		ac.All = append(ac.All[:i], ac.All[i+1:]...)
		if ba := removeAccount(ac.ByAddr[removed.Address], removed); len(ba) == 0 {
			delete(ac.ByAddr, removed.Address)
		} else {
			ac.ByAddr[removed.Address] = ba
		}
	}
}

func removeAccount(slice []accounts.Account, elem accounts.Account) []accounts.Account {
	for i := range slice {
		if slice[i] == elem {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

// find returns the cached account for address if there is a unique match.
// The exact matching rules are explained by the documentation of accounts.Account.
// Callers must hold ac.mu.
func (ac *AccountCache) Find(a accounts.Account) (accounts.Account, error) {
	// Limit search to address candidates if possible.
	matches := ac.All
	if (a.Address != common.Address{}) {
		matches = ac.ByAddr[a.Address]
	}
	if a.URL.Path != "" {
		// If only the basename is specified, complete the path.
		if !strings.ContainsRune(a.URL.Path, filepath.Separator) {
			a.URL.Path = filepath.Join(ac.keydir, a.URL.Path)
		}
		for i := range matches {
			if matches[i].URL == a.URL {
				return matches[i], nil
			}
		}
		if (a.Address == common.Address{}) {
			return accounts.Account{}, keystore.ErrNoMatch
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return accounts.Account{}, keystore.ErrNoMatch
	default:
		err := &AmbiguousAddrError{Addr: a.Address, Matches: make([]accounts.Account, len(matches))}
		copy(err.Matches, matches)
		sort.Sort(vault.AccountsByURL(err.Matches))
		return accounts.Account{}, err
	}
}

func (ac *AccountCache) MaybeReload() {
	ac.Mu.Lock()
	if ac.watcher.running {
		ac.Mu.Unlock()
		return // A watcher is running and will keep the cache up-to-date.
	}
	if ac.throttle == nil {
		ac.throttle = time.NewTimer(0)
	} else {
		select {
		case <-ac.throttle.C:
		default:
			ac.Mu.Unlock()
			return // The cache was reloaded recently.
		}
	}
	// No watcher running, start it.
	ac.watcher.start()
	ac.throttle.Reset(minReloadInterval)
	ac.Mu.Unlock()
	ac.scanAccounts()
}

func (ac *AccountCache) Close() {
	ac.Mu.Lock()
	ac.watcher.close()
	if ac.throttle != nil {
		ac.throttle.Stop()
	}
	if ac.notify != nil {
		close(ac.notify)
		ac.notify = nil
	}
	ac.Mu.Unlock()
}

// scanAccounts checks if any changes have occurred on the filesystem, and
// updates the account cache accordingly
func (ac *AccountCache) scanAccounts() error {
	// Scan the entire folder metadata for file changes
	creates, deletes, updates, err := ac.fileC.scan(ac.keydir)
	if err != nil {
		log.Debug("Failed to reload keystore contents", "err", err)
		return err
	}
	if creates.Cardinality() == 0 && deletes.Cardinality() == 0 && updates.Cardinality() == 0 {
		return nil
	}
	// Create a helper method to scan the contents of the key files
	var (
		buf        = new(bufio.Reader)
		acctConfig vault.ValidatableAccountGetterConfig
	)
	readAccount := func(path string) *accounts.Account {
		fd, err := os.Open(path)
		if err != nil {
			log.Trace("Failed to open keystore file", "path", path, "err", err)
			return nil
		}
		defer fd.Close()
		buf.Reset(fd)
		// Parse the address.
		acctConfig, err = ac.accountConfigUnmarshaller.Unmarshal(buf)
		if err != nil {
			log.Debug("Failed to decode keystore key", "path", path, "err", err)
			return nil
		}

		if err := acctConfig.Validate(); err != nil {
			log.Debug("Invalid secret config", "path", path, "err", err)
			return nil
		}

		return acctConfig.AsAccount(path)
	}
	// Process all the file diffs
	start := time.Now()

	for _, p := range creates.ToSlice() {
		if a := readAccount(p.(string)); a != nil {
			ac.Add(*a)

			if err := ac.unlockIfConfigured(*a); err != nil {
				log.Debug("Failed to unlock account", "path", p.(string), "err", err)
			}
		}
	}
	for _, p := range deletes.ToSlice() {
		ac.deleteByFile(p.(string))
	}
	for _, p := range updates.ToSlice() {
		path := p.(string)
		ac.deleteByFile(path)
		if a := readAccount(path); a != nil {
			ac.Add(*a)

			if err := ac.unlockIfConfigured(*a); err != nil {
				log.Debug("Failed to unlock account", "path", p.(string), "err", err)
			}
		}
	}

	end := time.Now()

	select {
	case ac.notify <- struct{}{}:
	default:
	}
	log.Trace("Handled keystore changes", "time", end.Sub(start))
	return nil
}

func (ac *AccountCache) unlockIfConfigured(acct accounts.Account) error {
	for _, toUnlock := range ac.toUnlock {
		if acct.Address == toUnlock {
			if err := ac.unlocker.TimedUnlock(acct, 0); err != nil {
				return err
			}
		}
	}
	return nil
}