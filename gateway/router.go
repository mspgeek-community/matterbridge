package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/mspgeek-community/matterbridge/bridge"
	"github.com/mspgeek-community/matterbridge/bridge/config"
	"github.com/mspgeek-community/matterbridge/gateway/samechannel"
	"github.com/sirupsen/logrus"
)

type Router struct {
	config.Config
	sync.RWMutex

	BridgeMap        map[string]bridge.Factory
	Gateways         map[string]*Gateway
	Message          chan config.Message
	MattermostPlugin chan config.Message

	store *msgStore

	logger *logrus.Entry
}

// NewRouter initializes a new Matterbridge router for the specified configuration and
// sets up all required gateways.
func NewRouter(rootLogger *logrus.Logger, cfg config.Config, bridgeMap map[string]bridge.Factory) (*Router, error) {
	logger := rootLogger.WithFields(logrus.Fields{"prefix": "router"})

	r := &Router{
		Config:           cfg,
		BridgeMap:        bridgeMap,
		Message:          make(chan config.Message),
		MattermostPlugin: make(chan config.Message),
		Gateways:         make(map[string]*Gateway),
		logger:           logger,
	}
	r.openMsgStore()
	sgw := samechannel.New(cfg)
	gwconfigs := append(sgw.GetConfig(), cfg.BridgeValues().Gateway...)

	for idx := range gwconfigs {
		entry := &gwconfigs[idx]
		if !entry.Enable {
			continue
		}
		if entry.Name == "" {
			return nil, fmt.Errorf("%s", "Gateway without name found")
		}
		if _, ok := r.Gateways[entry.Name]; ok {
			return nil, fmt.Errorf("Gateway with name %s already exists", entry.Name)
		}
		r.Gateways[entry.Name] = New(rootLogger, entry, r)
	}
	r.importLegacyCache()
	return r, nil
}

// openMsgStore opens the persistent message store. Failure is logged and the
// router runs with the in-memory cache only.
func (r *Router) openMsgStore() {
	general := r.BridgeValues().General
	path := general.MessageStore
	if path == "" {
		path = defaultMessageStorePath
	}
	if path == "-" {
		r.logger.Info("Persistent message store disabled")
		return
	}
	store, err := openMsgStore(path, r.logger)
	if err != nil {
		r.logger.Errorf("Opening message store %s failed, threads and edits will not survive a restart: %s", path, err)
		return
	}
	days := general.MessageStoreRetentionDays
	if days <= 0 {
		days = defaultMessageStoreRetentionDays
	}
	r.logger.Infof("Using message store %s (retention %d days)", path, days)
	r.store = store
	go store.pruneLoop(time.Duration(days) * 24 * time.Hour)
}

// Start will connect all gateways belonging to this router and subsequently route messages
// between them.
func (r *Router) Start() error {
	m := make(map[string]*bridge.Bridge)
	if len(r.Gateways) == 0 {
		return fmt.Errorf("no [[gateway]] configured. See https://github.com/mspgeek-community/matterbridge/wiki/How-to-create-your-config for more info")
	}
	for _, gw := range r.Gateways {
		r.logger.Debugf("Parsing gateway %s", gw.Name)
		if len(gw.Bridges) == 0 {
			return fmt.Errorf("no bridges configured for gateway %s. See https://github.com/mspgeek-community/matterbridge/wiki/How-to-create-your-config for more info", gw.Name)
		}
		for _, br := range gw.Bridges {
			m[br.Account] = br
		}
	}
	r.logger.Infof("Loaded %d gateways using %d bridges", len(r.Gateways), len(m))
	for _, br := range m {
		r.logger.Infof("Starting bridge: %s ", br.Account)
		err := br.Connect()
		if err != nil {
			e := fmt.Errorf("Bridge %s failed to start: %v", br.Account, err)
			if r.disableBridge(br, e) {
				continue
			}
			return e
		}
		r.logger.Infof("Bridge %s connected, joining %d channels", br.Account, len(br.Channels))
		err = br.JoinChannels()
		if err != nil {
			e := fmt.Errorf("Bridge %s failed to join channel: %v", br.Account, err)
			if r.disableBridge(br, e) {
				continue
			}
			return e
		}
		r.logger.Infof("Bridge %s started", br.Account)
	}
	// remove unused bridges
	for _, gw := range r.Gateways {
		for i, br := range gw.Bridges {
			if br.Bridger == nil {
				r.logger.Errorf("removing failed bridge %s", i)
				delete(gw.Bridges, i)
			}
		}
	}
	go r.handleReceive()
	//go r.updateChannelMembers()
	return nil
}

// disableBridge returns true and empties a bridge if we have IgnoreFailureOnStart configured
// otherwise returns false
func (r *Router) disableBridge(br *bridge.Bridge, err error) bool {
	if r.BridgeValues().General.IgnoreFailureOnStart {
		r.logger.Error(err)
		// setting this bridge empty
		*br = bridge.Bridge{
			Log: br.Log,
		}
		return true
	}
	return false
}

func (r *Router) getBridge(account string) *bridge.Bridge {
	for _, gw := range r.Gateways {
		if br, ok := gw.Bridges[account]; ok {
			return br
		}
	}
	return nil
}

func (r *Router) handleReceive() {
	for msg := range r.Message {
		msg := msg // scopelint
		r.handleEventGetChannelMembers(&msg)
		r.handleEventFailure(&msg)
		r.handleEventRejoinChannels(&msg)

		// Set message protocol based on the account it came from. A bridge
		// removed after failing to start may still have running goroutines
		// sending messages; drop those.
		srcBridge := r.getBridge(msg.Account)
		if srcBridge == nil {
			r.logger.Debugf("Dropping message from removed bridge %s", msg.Account)
			continue
		}
		msg.Protocol = srcBridge.Protocol

		filesHandled := false
		for _, gw := range r.Gateways {
			// record all the message ID's of the different bridges
			var msgIDs []*BrMsgID
			if gw.ignoreMessage(&msg) {
				continue
			}
			msg.Timestamp = time.Now()
			gw.modifyMessage(&msg)
			if !filesHandled {
				gw.handleFiles(&msg)
				filesHandled = true
			}
			for _, br := range gw.Bridges {
				msgIDs = append(msgIDs, gw.handleMessage(&msg, br)...)
			}

			// Only gateways that relayed the message record it: recording it in
			// every gateway filled each gateway's cache with other channels'
			// messages and evicted the mappings threads and edits need.
			if msg.ID != "" && len(msgIDs) > 0 {
				key := msg.Protocol + " " + msg.ID
				// Only add the message ID if it doesn't already exist
				if _, exists := gw.lookupIDs(key); !exists {
					gw.rememberIDs(key, msgIDs)
				}
			}
		}
	}
}

// updateChannelMembers sends every minute an GetChannelMembers event to all bridges.
func (r *Router) updateChannelMembers() {
	// TODO sleep a minute because slack can take a while
	// fix this by having actually connectionDone events send to the router
	time.Sleep(time.Minute)
	for {
		for _, gw := range r.Gateways {
			for _, br := range gw.Bridges {
				// only for slack now
				if br.Protocol != "slack" {
					continue
				}
				r.logger.Debugf("sending %s to %s", config.EventGetChannelMembers, br.Account)
				if _, err := br.Send(config.Message{Event: config.EventGetChannelMembers}); err != nil {
					r.logger.Errorf("updateChannelMembers: %s", err)
				}
			}
		}
		time.Sleep(time.Minute)
	}
}
