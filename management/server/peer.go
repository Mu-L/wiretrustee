package server

import (
	"context"
	"crypto/sha256"
	b64 "encoding/base64"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/xid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"

	nbdns "github.com/netbirdio/netbird/dns"
	"github.com/netbirdio/netbird/management/domain"
	"github.com/netbirdio/netbird/management/server/geolocation"
	"github.com/netbirdio/netbird/management/server/idp"
	routerTypes "github.com/netbirdio/netbird/management/server/networks/routers/types"
	"github.com/netbirdio/netbird/management/server/permissions/modules"
	"github.com/netbirdio/netbird/management/server/permissions/operations"
	"github.com/netbirdio/netbird/util"

	"github.com/netbirdio/netbird/management/server/posture"
	"github.com/netbirdio/netbird/management/server/store"
	"github.com/netbirdio/netbird/management/server/types"

	"github.com/netbirdio/netbird/management/proto"
	"github.com/netbirdio/netbird/management/server/activity"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"
	"github.com/netbirdio/netbird/management/server/status"
)

// GetPeers returns a list of peers under the given account filtering out peers that do not belong to a user if
// the current user is not an admin.
func (am *DefaultAccountManager) GetPeers(ctx context.Context, accountID, userID, nameFilter, ipFilter string) ([]*nbpeer.Peer, error) {
	user, err := am.Store.GetUserByUserID(ctx, store.LockingStrengthShare, userID)
	if err != nil {
		return nil, err
	}

	allowed, err := am.permissionsManager.ValidateUserPermissions(ctx, accountID, userID, modules.Peers, operations.Read)
	if err != nil {
		return nil, status.NewPermissionValidationError(err)
	}

	accountPeers, err := am.Store.GetAccountPeers(ctx, store.LockingStrengthShare, accountID, nameFilter, ipFilter)
	if err != nil {
		return nil, err
	}

	// @note if the user has permission to read peers it shows all account peers
	if allowed {
		return accountPeers, nil
	}

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to get account settings: %w", err)
	}

	if user.IsRestrictable() && settings.RegularUsersViewBlocked {
		return []*nbpeer.Peer{}, nil
	}

	// @note if it does not have permission read peers then only display it's own peers
	peers := make([]*nbpeer.Peer, 0)
	peersMap := make(map[string]*nbpeer.Peer)

	for _, peer := range accountPeers {
		if user.Id != peer.UserID {
			continue
		}
		peers = append(peers, peer)
		peersMap[peer.ID] = peer
	}

	return am.getUserAccessiblePeers(ctx, accountID, peersMap, peers)
}

func (am *DefaultAccountManager) getUserAccessiblePeers(ctx context.Context, accountID string, peersMap map[string]*nbpeer.Peer, peers []*nbpeer.Peer) ([]*nbpeer.Peer, error) {
	account, err := am.requestBuffer.GetAccountWithBackpressure(ctx, accountID)
	if err != nil {
		return nil, err
	}

	approvedPeersMap, err := am.integratedPeerValidator.GetValidatedPeers(ctx, accountID, maps.Values(account.Groups), maps.Values(account.Peers), account.Settings.Extra)
	if err != nil {
		return nil, err
	}

	// fetch all the peers that have access to the user's peers
	for _, peer := range peers {
		aclPeers, _ := account.GetPeerConnectionResources(ctx, peer, approvedPeersMap)
		for _, p := range aclPeers {
			peersMap[p.ID] = p
		}
	}

	return maps.Values(peersMap), nil
}

// MarkPeerConnected marks peer as connected (true) or disconnected (false)
func (am *DefaultAccountManager) MarkPeerConnected(ctx context.Context, peerPubKey string, connected bool, realIP net.IP, accountID string) error {
	start := time.Now()
	defer func() {
		log.WithContext(ctx).Debugf("MarkPeerConnected: took %v", time.Since(start))
	}()

	var peer *nbpeer.Peer
	var settings *types.Settings
	var expired bool
	var err error

	err = am.Store.ExecuteInTransaction(ctx, func(transaction store.Store) error {
		peer, err = transaction.GetPeerByPeerPubKey(ctx, store.LockingStrengthUpdate, peerPubKey)
		if err != nil {
			return err
		}

		expired, err = updatePeerStatusAndLocation(ctx, am.geo, transaction, peer, connected, realIP, accountID)
		return err
	})
	if err != nil {
		return err
	}

	if peer.AddedWithSSOLogin() {
		settings, err = am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
		if err != nil {
			return err
		}

		if peer.LoginExpirationEnabled && settings.PeerLoginExpirationEnabled {
			am.schedulePeerLoginExpiration(ctx, accountID)
		}

		if peer.InactivityExpirationEnabled && settings.PeerInactivityExpirationEnabled {
			am.checkAndSchedulePeerInactivityExpiration(ctx, accountID)
		}
	}

	if expired {
		// we need to update other peers because when peer login expires all other peers are notified to disconnect from
		// the expired one. Here we notify them that connection is now allowed again.
		am.BufferUpdateAccountPeers(ctx, accountID)
	}

	return nil
}

func updatePeerStatusAndLocation(ctx context.Context, geo geolocation.Geolocation, transaction store.Store, peer *nbpeer.Peer, connected bool, realIP net.IP, accountID string) (bool, error) {
	oldStatus := peer.Status.Copy()
	newStatus := oldStatus
	newStatus.LastSeen = time.Now().UTC()
	newStatus.Connected = connected
	// whenever peer got connected that means that it logged in successfully
	if newStatus.Connected {
		newStatus.LoginExpired = false
	}
	peer.Status = newStatus

	if geo != nil && realIP != nil {
		location, err := geo.Lookup(realIP)
		if err != nil {
			log.WithContext(ctx).Warnf("failed to get location for peer %s realip: [%s]: %v", peer.ID, realIP.String(), err)
		} else {
			peer.Location.ConnectionIP = realIP
			peer.Location.CountryCode = location.Country.ISOCode
			peer.Location.CityName = location.City.Names.En
			peer.Location.GeoNameID = location.City.GeonameID
			err = transaction.SavePeerLocation(ctx, store.LockingStrengthUpdate, accountID, peer)
			if err != nil {
				log.WithContext(ctx).Warnf("could not store location for peer %s: %s", peer.ID, err)
			}
		}
	}

	log.WithContext(ctx).Tracef("saving peer status for peer %s is connected: %t", peer.ID, connected)

	err := transaction.SavePeerStatus(ctx, store.LockingStrengthUpdate, accountID, peer.ID, *newStatus)
	if err != nil {
		return false, err
	}

	return oldStatus.LoginExpired, nil
}

// UpdatePeer updates peer. Only Peer.Name, Peer.SSHEnabled, Peer.LoginExpirationEnabled and Peer.InactivityExpirationEnabled can be updated.
func (am *DefaultAccountManager) UpdatePeer(ctx context.Context, accountID, userID string, update *nbpeer.Peer) (*nbpeer.Peer, error) {
	unlock := am.Store.AcquireWriteLockByUID(ctx, accountID)
	defer unlock()

	allowed, err := am.permissionsManager.ValidateUserPermissions(ctx, accountID, userID, modules.Peers, operations.Update)
	if err != nil {
		return nil, status.NewPermissionValidationError(err)
	}
	if !allowed {
		return nil, status.NewPermissionDeniedError()
	}

	var peer *nbpeer.Peer
	var settings *types.Settings
	var peerGroupList []string
	var requiresPeerUpdates bool
	var peerLabelChanged bool
	var sshChanged bool
	var loginExpirationChanged bool
	var inactivityExpirationChanged bool
	var dnsDomain string

	err = am.Store.ExecuteInTransaction(ctx, func(transaction store.Store) error {
		peer, err = transaction.GetPeerByID(ctx, store.LockingStrengthUpdate, accountID, update.ID)
		if err != nil {
			return err
		}

		settings, err = transaction.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
		if err != nil {
			return err
		}

		peerGroupList, err = getPeerGroupIDs(ctx, transaction, accountID, update.ID)
		if err != nil {
			return err
		}

		dnsDomain = am.GetDNSDomain(settings)

		update, requiresPeerUpdates, err = am.integratedPeerValidator.ValidatePeer(ctx, update, peer, userID, accountID, dnsDomain, peerGroupList, settings.Extra)
		if err != nil {
			return err
		}

		if peer.Name != update.Name {
			var newLabel string

			newLabel, err = nbdns.GetParsedDomainLabel(update.Name)
			if err != nil {
				newLabel = ""
			} else {
				_, err := transaction.GetPeerIdByLabel(ctx, store.LockingStrengthNone, accountID, update.Name)
				if err == nil {
					newLabel = ""
				}
			}

			if newLabel == "" {
				newLabel, err = getPeerIPDNSLabel(peer.IP, update.Name)
				if err != nil {
					return fmt.Errorf("failed to get free DNS label: %w", err)
				}
			}
			peer.Name = update.Name
			peer.DNSLabel = newLabel
			peerLabelChanged = true
		}

		if peer.SSHEnabled != update.SSHEnabled {
			peer.SSHEnabled = update.SSHEnabled
			sshChanged = true
		}

		if peer.LoginExpirationEnabled != update.LoginExpirationEnabled {
			if !peer.AddedWithSSOLogin() {
				return status.Errorf(status.PreconditionFailed, "this peer hasn't been added with the SSO login, therefore the login expiration can't be updated")
			}
			peer.LoginExpirationEnabled = update.LoginExpirationEnabled
			loginExpirationChanged = true
		}

		if peer.InactivityExpirationEnabled != update.InactivityExpirationEnabled {
			if !peer.AddedWithSSOLogin() {
				return status.Errorf(status.PreconditionFailed, "this peer hasn't been added with the SSO login, therefore the inactivity expiration can't be updated")
			}
			peer.InactivityExpirationEnabled = update.InactivityExpirationEnabled
			inactivityExpirationChanged = true
		}

		return transaction.SavePeer(ctx, store.LockingStrengthUpdate, accountID, peer)
	})
	if err != nil {
		return nil, err
	}

	if sshChanged {
		event := activity.PeerSSHEnabled
		if !peer.SSHEnabled {
			event = activity.PeerSSHDisabled
		}
		am.StoreEvent(ctx, userID, peer.IP.String(), accountID, event, peer.EventMeta(dnsDomain))
	}

	if peerLabelChanged {
		am.StoreEvent(ctx, userID, peer.ID, accountID, activity.PeerRenamed, peer.EventMeta(dnsDomain))
	}

	if loginExpirationChanged {
		event := activity.PeerLoginExpirationEnabled
		if !peer.LoginExpirationEnabled {
			event = activity.PeerLoginExpirationDisabled
		}
		am.StoreEvent(ctx, userID, peer.IP.String(), accountID, event, peer.EventMeta(dnsDomain))

		if peer.AddedWithSSOLogin() && peer.LoginExpirationEnabled && settings.PeerLoginExpirationEnabled {
			am.peerLoginExpiry.Cancel(ctx, []string{accountID})
			am.schedulePeerLoginExpiration(ctx, accountID)
		}
	}

	if inactivityExpirationChanged {
		event := activity.PeerInactivityExpirationEnabled
		if !peer.InactivityExpirationEnabled {
			event = activity.PeerInactivityExpirationDisabled
		}
		am.StoreEvent(ctx, userID, peer.IP.String(), accountID, event, peer.EventMeta(dnsDomain))

		if peer.AddedWithSSOLogin() && peer.InactivityExpirationEnabled && settings.PeerInactivityExpirationEnabled {
			am.checkAndSchedulePeerInactivityExpiration(ctx, accountID)
		}
	}

	if peerLabelChanged || requiresPeerUpdates {
		am.UpdateAccountPeers(ctx, accountID)
	} else if sshChanged {
		am.UpdateAccountPeer(ctx, accountID, peer.ID)
	}

	return peer, nil
}

// DeletePeer removes peer from the account by its IP
func (am *DefaultAccountManager) DeletePeer(ctx context.Context, accountID, peerID, userID string) error {
	unlock := am.Store.AcquireWriteLockByUID(ctx, accountID)
	defer unlock()

	allowed, err := am.permissionsManager.ValidateUserPermissions(ctx, accountID, userID, modules.Peers, operations.Delete)
	if err != nil {
		return status.NewPermissionValidationError(err)
	}
	if !allowed {
		return status.NewPermissionDeniedError()
	}

	peerAccountID, err := am.Store.GetAccountIDByPeerID(ctx, store.LockingStrengthShare, peerID)
	if err != nil {
		return err
	}

	if peerAccountID != accountID {
		return status.NewPeerNotPartOfAccountError()
	}

	var peer *nbpeer.Peer
	var updateAccountPeers bool
	var eventsToStore []func()

	err = am.Store.ExecuteInTransaction(ctx, func(transaction store.Store) error {
		peer, err = transaction.GetPeerByID(ctx, store.LockingStrengthUpdate, accountID, peerID)
		if err != nil {
			return err
		}

		if err = am.validatePeerDelete(ctx, transaction, accountID, peerID); err != nil {
			return err
		}

		updateAccountPeers, err = isPeerInActiveGroup(ctx, transaction, accountID, peerID)
		if err != nil {
			return err
		}

		if err = transaction.RemovePeerFromAllGroups(ctx, peer.ID); err != nil {
			return fmt.Errorf("failed to remove peer from groups: %w", err)
		}

		eventsToStore, err = deletePeers(ctx, am, transaction, accountID, userID, []*nbpeer.Peer{peer})
		if err != nil {
			return fmt.Errorf("failed to delete peer: %w", err)
		}

		if err = transaction.IncrementNetworkSerial(ctx, store.LockingStrengthUpdate, accountID); err != nil {
			return fmt.Errorf("failed to increment network serial: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, storeEvent := range eventsToStore {
		storeEvent()
	}

	if updateAccountPeers && userID != activity.SystemInitiator {
		am.BufferUpdateAccountPeers(ctx, accountID)
	}

	return nil
}

// GetNetworkMap returns Network map for a given peer (omits original peer from the Peers result)
func (am *DefaultAccountManager) GetNetworkMap(ctx context.Context, peerID string) (*types.NetworkMap, error) {
	account, err := am.Store.GetAccountByPeerID(ctx, peerID)
	if err != nil {
		return nil, err
	}

	peer := account.GetPeer(peerID)
	if peer == nil {
		return nil, status.Errorf(status.NotFound, "peer with ID %s not found", peerID)
	}

	groups := make(map[string][]string)
	for groupID, group := range account.Groups {
		groups[groupID] = group.Peers
	}

	validatedPeers, err := am.integratedPeerValidator.GetValidatedPeers(ctx, account.Id, maps.Values(account.Groups), maps.Values(account.Peers), account.Settings.Extra)
	if err != nil {
		return nil, err
	}
	customZone := account.GetPeersCustomZone(ctx, am.GetDNSDomain(account.Settings))

	proxyNetworkMaps, err := am.proxyController.GetProxyNetworkMaps(ctx, account.Id)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get proxy network maps: %v", err)
		return nil, err
	}

	networkMap := account.GetPeerNetworkMap(ctx, peer.ID, customZone, validatedPeers, account.GetResourcePoliciesMap(), account.GetResourceRoutersMap(), nil)

	proxyNetworkMap, ok := proxyNetworkMaps[peer.ID]
	if ok {
		networkMap.Merge(proxyNetworkMap)
	}

	return networkMap, nil
}

// GetPeerNetwork returns the Network for a given peer
func (am *DefaultAccountManager) GetPeerNetwork(ctx context.Context, peerID string) (*types.Network, error) {
	account, err := am.Store.GetAccountByPeerID(ctx, peerID)
	if err != nil {
		return nil, err
	}

	return account.Network.Copy(), err
}

// AddPeer adds a new peer to the Store.
// Each Account has a list of pre-authorized SetupKey and if no Account has a given key err with a code status.PermissionDenied
// will be returned, meaning the setup key is invalid or not found.
// If a User ID is provided, it means that we passed the authentication using JWT, then we look for account by User ID and register the peer
// to it. We also add the User ID to the peer metadata to identify registrant. If no userID provided, then fail with status.PermissionDenied
// Each new Peer will be assigned a new next net.IP from the Account.Network and Account.Network.LastIP will be updated (IP's are not reused).
// The peer property is just a placeholder for the Peer properties to pass further
func (am *DefaultAccountManager) AddPeer(ctx context.Context, setupKey, userID string, peer *nbpeer.Peer) (*nbpeer.Peer, *types.NetworkMap, []*posture.Checks, error) {
	if setupKey == "" && userID == "" {
		// no auth method provided => reject access
		return nil, nil, nil, status.Errorf(status.Unauthenticated, "no peer auth method provided, please use a setup key or interactive SSO login")
	}

	upperKey := strings.ToUpper(setupKey)
	hashedKey := sha256.Sum256([]byte(upperKey))
	encodedHashedKey := b64.StdEncoding.EncodeToString(hashedKey[:])
	addedByUser := len(userID) > 0

	// This is a handling for the case when the same machine (with the same WireGuard pub key) tries to register twice.
	// Such case is possible when AddPeer function takes long time to finish after AcquireWriteLockByUID (e.g., database is slow)
	// and the peer disconnects with a timeout and tries to register again.
	// We just check if this machine has been registered before and reject the second registration.
	// The connecting peer should be able to recover with a retry.
	_, err := am.Store.GetPeerByPeerPubKey(ctx, store.LockingStrengthNone, peer.Key)
	if err == nil {
		return nil, nil, nil, status.Errorf(status.PreconditionFailed, "peer has been already registered")
	}

	opEvent := &activity.Event{
		Timestamp: time.Now().UTC(),
	}

	var newPeer *nbpeer.Peer

	var setupKeyID string
	var setupKeyName string
	var ephemeral bool
	var groupsToAdd []string
	var allowExtraDNSLabels bool
	var accountID string
	var isEphemeral bool
	if addedByUser {
		user, err := am.Store.GetUserByUserID(ctx, store.LockingStrengthNone, userID)
		if err != nil {
			return nil, nil, nil, status.Errorf(status.NotFound, "failed adding new peer: user not found")
		}
		groupsToAdd = user.AutoGroups
		opEvent.InitiatorID = userID
		opEvent.Activity = activity.PeerAddedByUser
		accountID = user.AccountID
	} else {
		// Validate the setup key
		sk, err := am.Store.GetSetupKeyBySecret(ctx, store.LockingStrengthNone, encodedHashedKey)
		if err != nil {
			return nil, nil, nil, status.Errorf(status.NotFound, "couldn't add peer: setup key is invalid")
		}

		// we will check key twice for early return
		if !sk.IsValid() {
			return nil, nil, nil, status.Errorf(status.NotFound, "couldn't add peer: setup key is invalid")
		}

		opEvent.InitiatorID = sk.Id
		opEvent.Activity = activity.PeerAddedWithSetupKey
		groupsToAdd = sk.AutoGroups
		ephemeral = sk.Ephemeral
		setupKeyID = sk.Id
		setupKeyName = sk.Name
		allowExtraDNSLabels = sk.AllowExtraDNSLabels
		accountID = sk.AccountID
		isEphemeral = sk.Ephemeral
		if !sk.AllowExtraDNSLabels && len(peer.ExtraDNSLabels) > 0 {
			return nil, nil, nil, status.Errorf(status.PreconditionFailed, "couldn't add peer: setup key doesn't allow extra DNS labels")
		}
	}
	opEvent.AccountID = accountID

	if (strings.ToLower(peer.Meta.Hostname) == "iphone" || strings.ToLower(peer.Meta.Hostname) == "ipad") && userID != "" {
		if am.idpManager != nil {
			userdata, err := am.idpManager.GetUserDataByID(ctx, userID, idp.AppMetadata{WTAccountID: accountID})
			if err == nil && userdata != nil {
				peer.Meta.Hostname = fmt.Sprintf("%s-%s", peer.Meta.Hostname, strings.Split(userdata.Email, "@")[0])
			}
		}
	}

	if err := domain.ValidateDomainsList(peer.ExtraDNSLabels); err != nil {
		return nil, nil, nil, status.Errorf(status.InvalidArgument, "invalid extra DNS labels: %v", err)
	}

	registrationTime := time.Now().UTC()
	newPeer = &nbpeer.Peer{
		ID:                          xid.New().String(),
		AccountID:                   accountID,
		Key:                         peer.Key,
		Meta:                        peer.Meta,
		Name:                        peer.Meta.Hostname,
		UserID:                      userID,
		Status:                      &nbpeer.PeerStatus{Connected: false, LastSeen: registrationTime},
		SSHEnabled:                  false,
		SSHKey:                      peer.SSHKey,
		LastLogin:                   &registrationTime,
		CreatedAt:                   registrationTime,
		LoginExpirationEnabled:      addedByUser,
		Ephemeral:                   ephemeral,
		Location:                    peer.Location,
		InactivityExpirationEnabled: addedByUser,
		ExtraDNSLabels:              peer.ExtraDNSLabels,
		AllowExtraDNSLabels:         allowExtraDNSLabels,
	}
	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthNone, accountID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get account settings: %w", err)
	}

	if am.geo != nil && newPeer.Location.ConnectionIP != nil {
		location, err := am.geo.Lookup(newPeer.Location.ConnectionIP)
		if err != nil {
			log.WithContext(ctx).Warnf("failed to get location for new peer realip: [%s]: %v", newPeer.Location.ConnectionIP.String(), err)
		} else {
			newPeer.Location.CountryCode = location.Country.ISOCode
			newPeer.Location.CityName = location.City.Names.En
			newPeer.Location.GeoNameID = location.City.GeonameID
		}
	}

	newPeer = am.integratedPeerValidator.PreparePeer(ctx, accountID, newPeer, groupsToAdd, settings.Extra)

	network, err := am.Store.GetAccountNetwork(ctx, store.LockingStrengthNone, accountID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed getting network: %w", err)
	}

	maxAttempts := 10
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var freeIP net.IP
		freeIP, err = types.AllocateRandomPeerIP(network.Net)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to get free IP: %w", err)
		}

		var freeLabel string
		if isEphemeral || attempt > 1 {
			freeLabel, err = getPeerIPDNSLabel(freeIP, peer.Meta.Hostname)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to get free DNS label: %w", err)
			}
		} else {
			freeLabel, err = nbdns.GetParsedDomainLabel(peer.Meta.Hostname)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to get free DNS label: %w", err)
			}
		}
		newPeer.DNSLabel = freeLabel
		newPeer.IP = freeIP

		unlock := am.Store.AcquireWriteLockByUID(ctx, accountID)
		defer func() {
			if unlock != nil {
				unlock()
			}
		}()

		err = am.Store.ExecuteInTransaction(ctx, func(transaction store.Store) error {
			err = transaction.AddPeerToAccount(ctx, store.LockingStrengthUpdate, newPeer)
			if err != nil {
				return err
			}

			if len(groupsToAdd) > 0 {
				for _, g := range groupsToAdd {
					err = transaction.AddPeerToGroup(ctx, newPeer.AccountID, newPeer.ID, g)
					if err != nil {
						return err
					}
				}
			}

			err = transaction.AddPeerToAllGroup(ctx, accountID, newPeer.ID)
			if err != nil {
				return fmt.Errorf("failed adding peer to All group: %w", err)
			}

			if addedByUser {
				err := transaction.SaveUserLastLogin(ctx, accountID, userID, newPeer.GetLastLogin())
				if err != nil {
					log.WithContext(ctx).Debugf("failed to update user last login: %v", err)
				}
			} else {
				sk, err := transaction.GetSetupKeyBySecret(ctx, store.LockingStrengthUpdate, encodedHashedKey)
				if err != nil {
					return fmt.Errorf("failed to get setup key: %w", err)
				}

				// we validate at the end to not block the setup key for too long
				if !sk.IsValid() {
					return status.Errorf(status.PreconditionFailed, "couldn't add peer: setup key is invalid")
				}

				err = transaction.IncrementSetupKeyUsage(ctx, setupKeyID)
				if err != nil {
					return fmt.Errorf("failed to increment setup key usage: %w", err)
				}
			}

			err = transaction.IncrementNetworkSerial(ctx, store.LockingStrengthUpdate, accountID)
			if err != nil {
				return fmt.Errorf("failed to increment network serial: %w", err)
			}

			log.WithContext(ctx).Debugf("Peer %s added to account %s", newPeer.ID, accountID)
			return nil
		})
		if err == nil {
			unlock()
			unlock = nil
			break
		}

		if isUniqueConstraintError(err) {
			unlock()
			unlock = nil
			log.WithContext(ctx).WithFields(log.Fields{"dns_label": freeLabel, "ip": freeIP}).Tracef("Failed to add peer in attempt %d, retrying: %v", attempt, err)
			continue
		}

		return nil, nil, nil, fmt.Errorf("failed to add peer to database: %w", err)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to add peer to database after %d attempts: %w", maxAttempts, err)
	}

	updateAccountPeers, err := isPeerInActiveGroup(ctx, am.Store, accountID, newPeer.ID)
	if err != nil {
		updateAccountPeers = true
	}

	if newPeer == nil {
		return nil, nil, nil, fmt.Errorf("new peer is nil")
	}

	opEvent.TargetID = newPeer.ID
	opEvent.Meta = newPeer.EventMeta(am.GetDNSDomain(settings))
	if !addedByUser {
		opEvent.Meta["setup_key_name"] = setupKeyName
	}

	am.StoreEvent(ctx, opEvent.InitiatorID, opEvent.TargetID, opEvent.AccountID, opEvent.Activity, opEvent.Meta)

	if updateAccountPeers {
		am.BufferUpdateAccountPeers(ctx, accountID)
	}

	return am.getValidatedPeerWithMap(ctx, false, accountID, newPeer)
}

func getPeerIPDNSLabel(ip net.IP, peerHostName string) (string, error) {
	ip = ip.To4()

	dnsName, err := nbdns.GetParsedDomainLabel(peerHostName)
	if err != nil {
		return "", fmt.Errorf("failed to parse peer host name %s: %w", peerHostName, err)
	}

	return fmt.Sprintf("%s-%d-%d", dnsName, ip[2], ip[3]), nil
}

// SyncPeer checks whether peer is eligible for receiving NetworkMap (authenticated) and returns its NetworkMap if eligible
func (am *DefaultAccountManager) SyncPeer(ctx context.Context, sync types.PeerSync, accountID string) (*nbpeer.Peer, *types.NetworkMap, []*posture.Checks, error) {
	start := time.Now()
	defer func() {
		log.WithContext(ctx).Debugf("SyncPeer: took %v", time.Since(start))
	}()

	var peer *nbpeer.Peer
	var peerNotValid bool
	var isStatusChanged bool
	var updated bool
	var err error
	var postureChecks []*posture.Checks

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, nil, nil, err
	}

	err = am.Store.ExecuteInTransaction(ctx, func(transaction store.Store) error {
		peer, err = transaction.GetPeerByPeerPubKey(ctx, store.LockingStrengthUpdate, sync.WireGuardPubKey)
		if err != nil {
			return status.NewPeerNotRegisteredError()
		}

		if peer.UserID != "" {
			user, err := transaction.GetUserByUserID(ctx, store.LockingStrengthShare, peer.UserID)
			if err != nil {
				return err
			}

			if err = checkIfPeerOwnerIsBlocked(peer, user); err != nil {
				return err
			}
		}

		if peerLoginExpired(ctx, peer, settings) {
			return status.NewPeerLoginExpiredError()
		}

		peerGroupIDs, err := getPeerGroupIDs(ctx, transaction, accountID, peer.ID)
		if err != nil {
			return err
		}

		peerNotValid, isStatusChanged, err = am.integratedPeerValidator.IsNotValidPeer(ctx, accountID, peer, peerGroupIDs, settings.Extra)
		if err != nil {
			return err
		}

		updated = peer.UpdateMetaIfNew(sync.Meta)
		if updated {
			am.metrics.AccountManagerMetrics().CountPeerMetUpdate()
			log.WithContext(ctx).Tracef("peer %s metadata updated", peer.ID)
			if err = transaction.SavePeer(ctx, store.LockingStrengthUpdate, accountID, peer); err != nil {
				return err
			}

			postureChecks, err = getPeerPostureChecks(ctx, transaction, accountID, peer.ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if isStatusChanged || sync.UpdateAccountPeers || (updated && len(postureChecks) > 0) {
		am.BufferUpdateAccountPeers(ctx, accountID)
	}

	return am.getValidatedPeerWithMap(ctx, peerNotValid, accountID, peer)
}

func (am *DefaultAccountManager) handlePeerLoginNotFound(ctx context.Context, login types.PeerLogin, err error) (*nbpeer.Peer, *types.NetworkMap, []*posture.Checks, error) {
	if errStatus, ok := status.FromError(err); ok && errStatus.Type() == status.NotFound {
		// we couldn't find this peer by its public key which can mean that peer hasn't been registered yet.
		// Try registering it.
		newPeer := &nbpeer.Peer{
			Key:            login.WireGuardPubKey,
			Meta:           login.Meta,
			SSHKey:         login.SSHKey,
			Location:       nbpeer.Location{ConnectionIP: login.ConnectionIP},
			ExtraDNSLabels: login.ExtraDNSLabels,
		}

		return am.AddPeer(ctx, login.SetupKey, login.UserID, newPeer)
	}

	log.WithContext(ctx).Errorf("failed while logging in peer %s: %v", login.WireGuardPubKey, err)
	return nil, nil, nil, status.Errorf(status.Internal, "failed while logging in peer")
}

// LoginPeer logs in or registers a peer.
// If peer doesn't exist the function checks whether a setup key or a user is present and registers a new peer if so.
func (am *DefaultAccountManager) LoginPeer(ctx context.Context, login types.PeerLogin) (*nbpeer.Peer, *types.NetworkMap, []*posture.Checks, error) {
	accountID, err := am.Store.GetAccountIDByPeerPubKey(ctx, login.WireGuardPubKey)
	if err != nil {
		return am.handlePeerLoginNotFound(ctx, login, err)
	}

	// when the client sends a login request with a JWT which is used to get the user ID,
	// it means that the client has already checked if it needs login and had been through the SSO flow
	// so, we can skip this check and directly proceed with the login
	if login.UserID == "" {
		err = am.checkIFPeerNeedsLoginWithoutLock(ctx, accountID, login)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	unlockAccount := am.Store.AcquireReadLockByUID(ctx, accountID)
	defer unlockAccount()
	unlockPeer := am.Store.AcquireWriteLockByUID(ctx, login.WireGuardPubKey)
	defer func() {
		if unlockPeer != nil {
			unlockPeer()
		}
	}()

	var peer *nbpeer.Peer
	var updateRemotePeers bool
	var isRequiresApproval bool
	var isStatusChanged bool
	var isPeerUpdated bool
	var postureChecks []*posture.Checks

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, nil, nil, err
	}

	err = am.Store.ExecuteInTransaction(ctx, func(transaction store.Store) error {
		peer, err = transaction.GetPeerByPeerPubKey(ctx, store.LockingStrengthUpdate, login.WireGuardPubKey)
		if err != nil {
			return err
		}

		// this flag prevents unnecessary calls to the persistent store.
		shouldStorePeer := false

		if login.UserID != "" {
			if peer.UserID != login.UserID {
				log.Warnf("user mismatch when logging in peer %s: peer user %s, login user %s ", peer.ID, peer.UserID, login.UserID)
				return status.NewPeerLoginMismatchError()
			}

			changed, err := am.handleUserPeer(ctx, transaction, peer, settings)
			if err != nil {
				return err
			}

			if changed {
				shouldStorePeer = true
				updateRemotePeers = true
			}
		}

		peerGroupIDs, err := getPeerGroupIDs(ctx, transaction, accountID, peer.ID)
		if err != nil {
			return err
		}

		isRequiresApproval, isStatusChanged, err = am.integratedPeerValidator.IsNotValidPeer(ctx, accountID, peer, peerGroupIDs, settings.Extra)
		if err != nil {
			return err
		}

		isPeerUpdated = peer.UpdateMetaIfNew(login.Meta)
		if isPeerUpdated {
			am.metrics.AccountManagerMetrics().CountPeerMetUpdate()
			shouldStorePeer = true

			postureChecks, err = getPeerPostureChecks(ctx, transaction, accountID, peer.ID)
			if err != nil {
				return err
			}
		}

		if peer.SSHKey != login.SSHKey {
			peer.SSHKey = login.SSHKey
			shouldStorePeer = true
		}

		if !peer.AllowExtraDNSLabels && len(login.ExtraDNSLabels) > 0 {
			return status.Errorf(status.PreconditionFailed, "couldn't login peer: setup key doesn't allow extra DNS labels")
		}

		if shouldStorePeer {
			if err = transaction.SavePeer(ctx, store.LockingStrengthUpdate, accountID, peer); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	unlockPeer()
	unlockPeer = nil

	if updateRemotePeers || isStatusChanged || (isPeerUpdated && len(postureChecks) > 0) {
		am.BufferUpdateAccountPeers(ctx, accountID)
	}

	return am.getValidatedPeerWithMap(ctx, isRequiresApproval, accountID, peer)
}

// getPeerPostureChecks returns the posture checks for the peer.
func getPeerPostureChecks(ctx context.Context, transaction store.Store, accountID, peerID string) ([]*posture.Checks, error) {
	policies, err := transaction.GetAccountPolicies(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}

	if len(policies) == 0 {
		return nil, nil
	}

	var peerPostureChecksIDs []string

	for _, policy := range policies {
		if !policy.Enabled || len(policy.SourcePostureChecks) == 0 {
			continue
		}

		postureChecksIDs, err := processPeerPostureChecks(ctx, transaction, policy, accountID, peerID)
		if err != nil {
			return nil, err
		}

		peerPostureChecksIDs = append(peerPostureChecksIDs, postureChecksIDs...)
	}

	peerPostureChecks, err := transaction.GetPostureChecksByIDs(ctx, store.LockingStrengthShare, accountID, peerPostureChecksIDs)
	if err != nil {
		return nil, err
	}

	return maps.Values(peerPostureChecks), nil
}

// processPeerPostureChecks checks if the peer is in the source group of the policy and returns the posture checks.
func processPeerPostureChecks(ctx context.Context, transaction store.Store, policy *types.Policy, accountID, peerID string) ([]string, error) {
	for _, rule := range policy.Rules {
		if !rule.Enabled {
			continue
		}

		sourceGroups, err := transaction.GetGroupsByIDs(ctx, store.LockingStrengthShare, accountID, rule.Sources)
		if err != nil {
			return nil, err
		}

		for _, sourceGroup := range rule.Sources {
			group, ok := sourceGroups[sourceGroup]
			if !ok {
				return nil, fmt.Errorf("failed to check peer in policy source group")
			}

			if slices.Contains(group.Peers, peerID) {
				return policy.SourcePostureChecks, nil
			}
		}
	}
	return nil, nil
}

// checkIFPeerNeedsLoginWithoutLock checks if the peer needs login without acquiring the account lock. The check validate if the peer was not added via SSO
// and if the peer login is expired.
// The NetBird client doesn't have a way to check if the peer needs login besides sending a login request
// with no JWT token and usually no setup-key. As the client can send up to two login request to check if it is expired
// and before starting the engine, we do the checks without an account lock to avoid piling up requests.
func (am *DefaultAccountManager) checkIFPeerNeedsLoginWithoutLock(ctx context.Context, accountID string, login types.PeerLogin) error {
	peer, err := am.Store.GetPeerByPeerPubKey(ctx, store.LockingStrengthShare, login.WireGuardPubKey)
	if err != nil {
		return err
	}

	// if the peer was not added with SSO login we can exit early because peers activated with setup-key
	// doesn't expire, and we avoid extra databases calls.
	if !peer.AddedWithSSOLogin() {
		return nil
	}

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return err
	}

	if peerLoginExpired(ctx, peer, settings) {
		return status.NewPeerLoginExpiredError()
	}

	return nil
}

func (am *DefaultAccountManager) getValidatedPeerWithMap(ctx context.Context, isRequiresApproval bool, accountID string, peer *nbpeer.Peer) (*nbpeer.Peer, *types.NetworkMap, []*posture.Checks, error) {
	start := time.Now()
	defer func() {
		log.WithContext(ctx).Debugf("getValidatedPeerWithMap: took %s", time.Since(start))
	}()

	if isRequiresApproval {
		network, err := am.Store.GetAccountNetwork(ctx, store.LockingStrengthNone, accountID)
		if err != nil {
			return nil, nil, nil, err
		}

		emptyMap := &types.NetworkMap{
			Network: network.Copy(),
		}
		return peer, emptyMap, nil, nil
	}

	account, err := am.requestBuffer.GetAccountWithBackpressure(ctx, accountID)
	if err != nil {
		return nil, nil, nil, err
	}

	approvedPeersMap, err := am.integratedPeerValidator.GetValidatedPeers(ctx, account.Id, maps.Values(account.Groups), maps.Values(account.Peers), account.Settings.Extra)
	if err != nil {
		return nil, nil, nil, err
	}

	postureChecks, err := am.getPeerPostureChecks(account, peer.ID)
	if err != nil {
		return nil, nil, nil, err
	}

	customZone := account.GetPeersCustomZone(ctx, am.GetDNSDomain(account.Settings))

	proxyNetworkMaps, err := am.proxyController.GetProxyNetworkMaps(ctx, account.Id)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get proxy network maps: %v", err)
		return nil, nil, nil, err
	}

	networkMap := account.GetPeerNetworkMap(ctx, peer.ID, customZone, approvedPeersMap, account.GetResourcePoliciesMap(), account.GetResourceRoutersMap(), am.metrics.AccountManagerMetrics())

	proxyNetworkMap, ok := proxyNetworkMaps[peer.ID]
	if ok {
		networkMap.Merge(proxyNetworkMap)
	}

	return peer, networkMap, postureChecks, nil
}

func (am *DefaultAccountManager) handleExpiredPeer(ctx context.Context, transaction store.Store, user *types.User, peer *nbpeer.Peer) error {
	err := checkAuth(ctx, user.Id, peer)
	if err != nil {
		return err
	}
	// If peer was expired before and if it reached this point, it is re-authenticated.
	// UserID is present, meaning that JWT validation passed successfully in the API layer.
	peer = peer.UpdateLastLogin()
	err = transaction.SavePeer(ctx, store.LockingStrengthUpdate, peer.AccountID, peer)
	if err != nil {
		return err
	}

	err = transaction.SaveUserLastLogin(ctx, user.AccountID, user.Id, peer.GetLastLogin())
	if err != nil {
		log.WithContext(ctx).Debugf("failed to update user last login: %v", err)
	}

	settings, err := transaction.GetAccountSettings(ctx, store.LockingStrengthShare, peer.AccountID)
	if err != nil {
		return fmt.Errorf("failed to get account settings: %w", err)
	}

	am.StoreEvent(ctx, user.Id, peer.ID, user.AccountID, activity.UserLoggedInPeer, peer.EventMeta(am.GetDNSDomain(settings)))
	return nil
}

func checkIfPeerOwnerIsBlocked(peer *nbpeer.Peer, user *types.User) error {
	if peer.AddedWithSSOLogin() {
		if user.IsBlocked() {
			return status.Errorf(status.PermissionDenied, "user is blocked")
		}
	}
	return nil
}

func checkAuth(ctx context.Context, loginUserID string, peer *nbpeer.Peer) error {
	if loginUserID == "" {
		// absence of a user ID indicates that JWT wasn't provided.
		return status.NewPeerLoginExpiredError()
	}
	if peer.UserID != loginUserID {
		log.WithContext(ctx).Warnf("user mismatch when logging in peer %s: peer user %s, login user %s ", peer.ID, peer.UserID, loginUserID)
		return status.NewPeerLoginMismatchError()
	}
	return nil
}

func peerLoginExpired(ctx context.Context, peer *nbpeer.Peer, settings *types.Settings) bool {
	expired, expiresIn := peer.LoginExpired(settings.PeerLoginExpiration)
	expired = settings.PeerLoginExpirationEnabled && expired
	if expired || peer.Status.LoginExpired {
		log.WithContext(ctx).Debugf("peer's %s login expired %v ago", peer.ID, expiresIn)
		return true
	}
	return false
}

// GetPeer for a given accountID, peerID and userID error if not found.
func (am *DefaultAccountManager) GetPeer(ctx context.Context, accountID, peerID, userID string) (*nbpeer.Peer, error) {
	peer, err := am.Store.GetPeerByID(ctx, store.LockingStrengthShare, accountID, peerID)
	if err != nil {
		return nil, err
	}

	allowed, err := am.permissionsManager.ValidateUserPermissions(ctx, accountID, userID, modules.Peers, operations.Read)
	if err != nil {
		return nil, status.NewPermissionValidationError(err)
	}
	if allowed {
		return peer, nil
	}

	user, err := am.Store.GetUserByUserID(ctx, store.LockingStrengthShare, userID)
	if err != nil {
		return nil, err
	}

	// if admin or user owns this peer, return peer
	if user.IsAdminOrServiceUser() || peer.UserID == userID {
		return peer, nil
	}

	return am.checkIfUserOwnsPeer(ctx, accountID, userID, peer)
}

func (am *DefaultAccountManager) checkIfUserOwnsPeer(ctx context.Context, accountID, userID string, peer *nbpeer.Peer) (*nbpeer.Peer, error) {
	account, err := am.requestBuffer.GetAccountWithBackpressure(ctx, accountID)
	if err != nil {
		return nil, err
	}

	approvedPeersMap, err := am.integratedPeerValidator.GetValidatedPeers(ctx, accountID, maps.Values(account.Groups), maps.Values(account.Peers), account.Settings.Extra)
	if err != nil {
		return nil, err
	}

	// it is also possible that user doesn't own the peer but some of his peers have access to it,
	// this is a valid case, show the peer as well.
	userPeers, err := am.Store.GetUserPeers(ctx, store.LockingStrengthShare, accountID, userID)
	if err != nil {
		return nil, err
	}

	for _, p := range userPeers {
		aclPeers, _ := account.GetPeerConnectionResources(ctx, p, approvedPeersMap)
		for _, aclPeer := range aclPeers {
			if aclPeer.ID == peer.ID {
				return peer, nil
			}
		}
	}

	return nil, status.Errorf(status.Internal, "user %s has no access to peer %s under account %s", userID, peer.ID, accountID)
}

// UpdateAccountPeers updates all peers that belong to an account.
// Should be called when changes have to be synced to peers.
func (am *DefaultAccountManager) UpdateAccountPeers(ctx context.Context, accountID string) {
	log.WithContext(ctx).Tracef("updating peers for account %s from %s", accountID, util.GetCallerName())

	account, err := am.requestBuffer.GetAccountWithBackpressure(ctx, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to send out updates to peers. failed to get account: %v", err)
		return
	}

	globalStart := time.Now()

	hasPeersConnected := false
	for _, peer := range account.Peers {
		if am.peersUpdateManager.HasChannel(peer.ID) {
			hasPeersConnected = true
			break
		}

	}

	if !hasPeersConnected {
		return
	}

	approvedPeersMap, err := am.integratedPeerValidator.GetValidatedPeers(ctx, account.Id, maps.Values(account.Groups), maps.Values(account.Peers), account.Settings.Extra)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to send out updates to peers, failed to get validate peers: %v", err)
		return
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10)

	dnsCache := &DNSConfigCache{}
	dnsDomain := am.GetDNSDomain(account.Settings)
	customZone := account.GetPeersCustomZone(ctx, dnsDomain)
	resourcePolicies := account.GetResourcePoliciesMap()
	routers := account.GetResourceRoutersMap()

	proxyNetworkMaps, err := am.proxyController.GetProxyNetworkMaps(ctx, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get proxy network maps: %v", err)
		return
	}

	extraSetting, err := am.settingsManager.GetExtraSettings(ctx, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get flow enabled status: %v", err)
		return
	}

	for _, peer := range account.Peers {
		if !am.peersUpdateManager.HasChannel(peer.ID) {
			log.WithContext(ctx).Tracef("peer %s doesn't have a channel, skipping network map update", peer.ID)
			continue
		}

		wg.Add(1)
		semaphore <- struct{}{}
		go func(p *nbpeer.Peer) {
			defer wg.Done()
			defer func() { <-semaphore }()

			start := time.Now()

			postureChecks, err := am.getPeerPostureChecks(account, p.ID)
			if err != nil {
				log.WithContext(ctx).Debugf("failed to get posture checks for peer %s: %v", peer.ID, err)
				return
			}

			am.metrics.UpdateChannelMetrics().CountCalcPostureChecksDuration(time.Since(start))
			start = time.Now()

			remotePeerNetworkMap := account.GetPeerNetworkMap(ctx, p.ID, customZone, approvedPeersMap, resourcePolicies, routers, am.metrics.AccountManagerMetrics())

			am.metrics.UpdateChannelMetrics().CountCalcPeerNetworkMapDuration(time.Since(start))
			start = time.Now()

			proxyNetworkMap, ok := proxyNetworkMaps[p.ID]
			if ok {
				remotePeerNetworkMap.Merge(proxyNetworkMap)
			}
			am.metrics.UpdateChannelMetrics().CountMergeNetworkMapDuration(time.Since(start))

			start = time.Now()
			update := toSyncResponse(ctx, nil, p, nil, nil, remotePeerNetworkMap, dnsDomain, postureChecks, dnsCache, account.Settings, extraSetting)
			am.metrics.UpdateChannelMetrics().CountToSyncResponseDuration(time.Since(start))

			am.peersUpdateManager.SendUpdate(ctx, p.ID, &UpdateMessage{Update: update, NetworkMap: remotePeerNetworkMap})
		}(peer)
	}

	//

	wg.Wait()
	if am.metrics != nil {
		am.metrics.AccountManagerMetrics().CountUpdateAccountPeersDuration(time.Since(globalStart))
	}
}

type bufferUpdate struct {
	mu     sync.Mutex
	next   *time.Timer
	update atomic.Bool
}

func (am *DefaultAccountManager) BufferUpdateAccountPeers(ctx context.Context, accountID string) {
	log.WithContext(ctx).Tracef("buffer updating peers for account %s from %s", accountID, util.GetCallerName())

	bufUpd, _ := am.accountUpdateLocks.LoadOrStore(accountID, &bufferUpdate{})
	b := bufUpd.(*bufferUpdate)

	if !b.mu.TryLock() {
		b.update.Store(true)
		return
	}

	if b.next != nil {
		b.next.Stop()
	}

	go func() {
		defer b.mu.Unlock()
		am.UpdateAccountPeers(ctx, accountID)
		if !b.update.Load() {
			return
		}
		b.update.Store(false)
		if b.next == nil {
			b.next = time.AfterFunc(time.Duration(am.updateAccountPeersBufferInterval.Load()), func() {
				am.UpdateAccountPeers(ctx, accountID)
			})
			return
		}
		b.next.Reset(time.Duration(am.updateAccountPeersBufferInterval.Load()))
	}()
}

// UpdateAccountPeer updates a single peer that belongs to an account.
// Should be called when changes need to be synced to a specific peer only.
func (am *DefaultAccountManager) UpdateAccountPeer(ctx context.Context, accountId string, peerId string) {
	if !am.peersUpdateManager.HasChannel(peerId) {
		log.WithContext(ctx).Tracef("peer %s doesn't have a channel, skipping network map update", peerId)
		return
	}

	account, err := am.requestBuffer.GetAccountWithBackpressure(ctx, accountId)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to send out updates to peer %s. failed to get account: %v", peerId, err)
		return
	}

	peer := account.GetPeer(peerId)
	if peer == nil {
		log.WithContext(ctx).Tracef("peer %s  doesn't exists in account %s", peerId, accountId)
		return
	}

	approvedPeersMap, err := am.integratedPeerValidator.GetValidatedPeers(ctx, account.Id, maps.Values(account.Groups), maps.Values(account.Peers), account.Settings.Extra)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to send update to peer %s, failed to validate peers: %v", peerId, err)
		return
	}

	dnsCache := &DNSConfigCache{}
	dnsDomain := am.GetDNSDomain(account.Settings)
	customZone := account.GetPeersCustomZone(ctx, dnsDomain)
	resourcePolicies := account.GetResourcePoliciesMap()
	routers := account.GetResourceRoutersMap()

	postureChecks, err := am.getPeerPostureChecks(account, peerId)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to send update to peer %s, failed to get posture checks: %v", peerId, err)
		return
	}

	proxyNetworkMaps, err := am.proxyController.GetProxyNetworkMaps(ctx, accountId)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get proxy network maps: %v", err)
		return
	}

	remotePeerNetworkMap := account.GetPeerNetworkMap(ctx, peerId, customZone, approvedPeersMap, resourcePolicies, routers, am.metrics.AccountManagerMetrics())

	proxyNetworkMap, ok := proxyNetworkMaps[peer.ID]
	if ok {
		remotePeerNetworkMap.Merge(proxyNetworkMap)
	}

	extraSettings, err := am.settingsManager.GetExtraSettings(ctx, peer.AccountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get extra settings: %v", err)
		return
	}

	update := toSyncResponse(ctx, nil, peer, nil, nil, remotePeerNetworkMap, dnsDomain, postureChecks, dnsCache, account.Settings, extraSettings)
	am.peersUpdateManager.SendUpdate(ctx, peer.ID, &UpdateMessage{Update: update, NetworkMap: remotePeerNetworkMap})
}

// getNextPeerExpiration returns the minimum duration in which the next peer of the account will expire if it was found.
// If there is no peer that expires this function returns false and a duration of 0.
// This function only considers peers that haven't been expired yet and that are connected.
func (am *DefaultAccountManager) getNextPeerExpiration(ctx context.Context, accountID string) (time.Duration, bool) {
	peersWithExpiry, err := am.Store.GetAccountPeersWithExpiration(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get peers with expiration: %v", err)
		return peerSchedulerRetryInterval, true
	}

	if len(peersWithExpiry) == 0 {
		return 0, false
	}

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get account settings: %v", err)
		return peerSchedulerRetryInterval, true
	}

	var nextExpiry *time.Duration
	for _, peer := range peersWithExpiry {
		// consider only connected peers because others will require login on connecting to the management server
		if peer.Status.LoginExpired || !peer.Status.Connected {
			continue
		}
		_, duration := peer.LoginExpired(settings.PeerLoginExpiration)
		if nextExpiry == nil || duration < *nextExpiry {
			// if expiration is below 1s return 1s duration
			// this avoids issues with ticker that can't be set to < 0
			if duration < time.Second {
				return time.Second, true
			}
			nextExpiry = &duration
		}
	}

	if nextExpiry == nil {
		return 0, false
	}

	return *nextExpiry, true
}

// GetNextInactivePeerExpiration returns the minimum duration in which the next peer of the account will expire if it was found.
// If there is no peer that expires this function returns false and a duration of 0.
// This function only considers peers that haven't been expired yet and that are not connected.
func (am *DefaultAccountManager) getNextInactivePeerExpiration(ctx context.Context, accountID string) (time.Duration, bool) {
	peersWithInactivity, err := am.Store.GetAccountPeersWithInactivity(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get peers with inactivity: %v", err)
		return peerSchedulerRetryInterval, true
	}

	if len(peersWithInactivity) == 0 {
		return 0, false
	}

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("failed to get account settings: %v", err)
		return peerSchedulerRetryInterval, true
	}

	var nextExpiry *time.Duration
	for _, peer := range peersWithInactivity {
		if peer.Status.LoginExpired || peer.Status.Connected {
			continue
		}
		_, duration := peer.SessionExpired(settings.PeerInactivityExpiration)
		if nextExpiry == nil || duration < *nextExpiry {
			// if expiration is below 1s return 1s duration
			// this avoids issues with ticker that can't be set to < 0
			if duration < time.Second {
				return time.Second, true
			}
			nextExpiry = &duration
		}
	}

	if nextExpiry == nil {
		return 0, false
	}

	return *nextExpiry, true
}

// getExpiredPeers returns peers that have been expired.
func (am *DefaultAccountManager) getExpiredPeers(ctx context.Context, accountID string) ([]*nbpeer.Peer, error) {
	peersWithExpiry, err := am.Store.GetAccountPeersWithExpiration(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}

	var peers []*nbpeer.Peer
	for _, peer := range peersWithExpiry {
		expired, _ := peer.LoginExpired(settings.PeerLoginExpiration)
		if expired {
			peers = append(peers, peer)
		}
	}

	return peers, nil
}

// getInactivePeers returns peers that have been expired by inactivity
func (am *DefaultAccountManager) getInactivePeers(ctx context.Context, accountID string) ([]*nbpeer.Peer, error) {
	peersWithInactivity, err := am.Store.GetAccountPeersWithInactivity(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}

	settings, err := am.Store.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}

	var peers []*nbpeer.Peer
	for _, inactivePeer := range peersWithInactivity {
		inactive, _ := inactivePeer.SessionExpired(settings.PeerInactivityExpiration)
		if inactive {
			peers = append(peers, inactivePeer)
		}
	}

	return peers, nil
}

// GetPeerGroups returns groups that the peer is part of.
func (am *DefaultAccountManager) GetPeerGroups(ctx context.Context, accountID, peerID string) ([]*types.Group, error) {
	return am.Store.GetPeerGroups(ctx, store.LockingStrengthShare, accountID, peerID)
}

// getPeerGroupIDs returns the IDs of the groups that the peer is part of.
func getPeerGroupIDs(ctx context.Context, transaction store.Store, accountID string, peerID string) ([]string, error) {
	return transaction.GetPeerGroupIDs(ctx, store.LockingStrengthShare, accountID, peerID)
}

// IsPeerInActiveGroup checks if the given peer is part of a group that is used
// in an active DNS, route, or ACL configuration.
func isPeerInActiveGroup(ctx context.Context, transaction store.Store, accountID, peerID string) (bool, error) {
	peerGroupIDs, err := getPeerGroupIDs(ctx, transaction, accountID, peerID)
	if err != nil {
		return false, err
	}
	return areGroupChangesAffectPeers(ctx, transaction, accountID, peerGroupIDs) // TODO: use transaction
}

// deletePeers deletes all specified peers and sends updates to the remote peers.
// Returns a slice of functions to save events after successful peer deletion.
func deletePeers(ctx context.Context, am *DefaultAccountManager, transaction store.Store, accountID, userID string, peers []*nbpeer.Peer) ([]func(), error) {
	var peerDeletedEvents []func()

	settings, err := transaction.GetAccountSettings(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}
	dnsDomain := am.GetDNSDomain(settings)

	network, err := transaction.GetAccountNetwork(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		return nil, err
	}

	for _, peer := range peers {
		if err := transaction.RemovePeerFromAllGroups(ctx, peer.ID); err != nil {
			return nil, fmt.Errorf("failed to remove peer %s from groups", peer.ID)
		}

		if err := am.integratedPeerValidator.PeerDeleted(ctx, accountID, peer.ID, settings.Extra); err != nil {
			return nil, err
		}

		if err = transaction.DeletePeer(ctx, store.LockingStrengthUpdate, accountID, peer.ID); err != nil {
			return nil, err
		}

		am.peersUpdateManager.SendUpdate(ctx, peer.ID, &UpdateMessage{
			Update: &proto.SyncResponse{
				RemotePeers:        []*proto.RemotePeerConfig{},
				RemotePeersIsEmpty: true,
				NetworkMap: &proto.NetworkMap{
					Serial:               network.CurrentSerial(),
					RemotePeers:          []*proto.RemotePeerConfig{},
					RemotePeersIsEmpty:   true,
					FirewallRules:        []*proto.FirewallRule{},
					FirewallRulesIsEmpty: true,
				},
			},
			NetworkMap: &types.NetworkMap{},
		})
		am.peersUpdateManager.CloseChannel(ctx, peer.ID)
		peerDeletedEvents = append(peerDeletedEvents, func() {
			am.StoreEvent(ctx, userID, peer.ID, accountID, activity.PeerRemovedByUser, peer.EventMeta(dnsDomain))
		})
	}

	return peerDeletedEvents, nil
}

func ConvertSliceToMap(existingLabels []string) map[string]struct{} {
	labelMap := make(map[string]struct{}, len(existingLabels))
	for _, label := range existingLabels {
		labelMap[label] = struct{}{}
	}
	return labelMap
}

// validatePeerDelete checks if the peer can be deleted.
func (am *DefaultAccountManager) validatePeerDelete(ctx context.Context, transaction store.Store, accountId, peerId string) error {
	linkedInIngressPorts, err := am.proxyController.IsPeerInIngressPorts(ctx, accountId, peerId)
	if err != nil {
		return err
	}

	if linkedInIngressPorts {
		return status.Errorf(status.PreconditionFailed, "peer is linked to ingress ports: %s", peerId)
	}

	linked, router := isPeerLinkedToNetworkRouter(ctx, transaction, accountId, peerId)
	if linked {
		return status.Errorf(status.PreconditionFailed, "peer is linked to a network router: %s", router.ID)
	}

	return nil
}

// isPeerLinkedToNetworkRouter checks if a peer is linked to any network router in the account.
func isPeerLinkedToNetworkRouter(ctx context.Context, transaction store.Store, accountID string, peerID string) (bool, *routerTypes.NetworkRouter) {
	routers, err := transaction.GetNetworkRoutersByAccountID(ctx, store.LockingStrengthShare, accountID)
	if err != nil {
		log.WithContext(ctx).Errorf("error retrieving network routers while checking peer linkage: %v", err)
		return false, nil
	}

	for _, router := range routers {
		if router.Peer == peerID {
			return true, router
		}
	}

	return false, nil
}
