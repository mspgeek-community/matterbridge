package gateway

import (
	"encoding/json"
	"os"
	"strings"
)

const legacyCachePath = "/etc/matterbridge/cache.json"

// legacyCacheData is the format of the cache.json file written by earlier
// versions. Every gateway wrote its cache to the same file, so it only holds
// the mappings of whichever gateway wrote last.
type legacyCacheData struct {
	Items map[string][]struct {
		BridgeName string
		ID         string
		ChannelID  string
	}
}

// importLegacyCache copies the mappings in cache.json into the message store
// once, assigning each to the gateway that owns its channel, and renames the
// file so it is not imported again.
func (r *Router) importLegacyCache() {
	if r.store == nil {
		return
	}
	data, err := os.ReadFile(legacyCachePath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		r.logger.Errorf("Reading %s failed: %s", legacyCachePath, err)
		return
	}
	var cache legacyCacheData
	if err := json.Unmarshal(data, &cache); err != nil {
		r.logger.Errorf("Parsing %s failed, not importing it: %s", legacyCachePath, err)
		return
	}

	imported := 0
	for key, items := range cache.Items {
		for _, gw := range r.Gateways {
			var ids []storedMsgID
			for _, item := range items {
				if _, ok := gw.Channels[item.ChannelID]; !ok {
					continue
				}
				// IDs are stored as "<protocol> <id>"; accounts are "<protocol>.<name>".
				protocol, _, ok := strings.Cut(item.ID, " ")
				if !ok {
					continue
				}
				account := protocol + "." + item.BridgeName
				if _, ok := gw.Bridges[account]; !ok {
					continue
				}
				ids = append(ids, storedMsgID{Account: account, ID: item.ID, ChannelID: item.ChannelID})
			}
			if len(ids) == 0 {
				continue
			}
			if err := r.store.save(gw.Name, key, ids); err != nil {
				r.logger.Errorf("Importing %s failed: %s", key, err)
				continue
			}
			imported++
		}
	}
	r.logger.Infof("Imported %d message mappings from %s", imported, legacyCachePath)
	if err := os.Rename(legacyCachePath, legacyCachePath+".imported"); err != nil {
		r.logger.Errorf("Renaming %s failed: %s", legacyCachePath, err)
	}
}
