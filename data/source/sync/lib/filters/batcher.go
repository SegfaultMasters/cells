/*
 * Copyright (c) 2018. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

package filters

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	common2 "github.com/pydio/cells/common"

	"go.uber.org/zap"

	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/proto/tree"
	"github.com/pydio/cells/data/source/sync/lib/common"
)

type EventsBatcher struct {
	Source        common.PathSyncSource
	Target        common.PathSyncTarget
	globalContext context.Context

	batchCacheMutex *sync.Mutex
	batchCache      map[string][]common.EventInfo

	batchOut         chan *Batch
	eventChannels    []chan common.ProcessorEvent
	closeSessionChan chan string
}

func (ev *EventsBatcher) RegisterEventChannel(out chan common.ProcessorEvent) {
	ev.eventChannels = append(ev.eventChannels, out)
}

func (ev *EventsBatcher) sendEvent(event common.ProcessorEvent) {
	for _, channel := range ev.eventChannels {
		channel <- event
	}
}

func (ev *EventsBatcher) FilterBatch(batch *Batch) {

	ev.sendEvent(common.ProcessorEvent{
		Type: "filter:start",
		Data: batch,
	})
	for _, createEvent := range batch.CreateFiles {
		var node *tree.Node
		var err error
		if createEvent.EventInfo.ScanEvent && createEvent.EventInfo.ScanSourceNode != nil {
			node = createEvent.EventInfo.ScanSourceNode
			log.Logger(ev.globalContext).Debug("Create File", node.Zap())
		} else {
			// Todo : Feed node from event instead of calling LoadNode() again?
			node, err = ev.Source.LoadNode(createEvent.EventInfo.CreateContext(ev.globalContext), createEvent.EventInfo.Path)
			log.Logger(ev.globalContext).Debug("Load File", node.Zap())
		}
		if err != nil {
			delete(batch.CreateFiles, createEvent.Key)
			if _, exists := batch.Deletes[createEvent.Key]; exists {
				delete(batch.Deletes, createEvent.Key)
			}
		} else {
			createEvent.Node = node
			if node.Uuid == "" && path.Base(node.Path) != common2.PYDIO_SYNC_HIDDEN_FILE_META {
				batch.RefreshFilesUuid[createEvent.Key] = createEvent
			}
		}
	}

	for _, createEvent := range batch.CreateFolders {
		var node *tree.Node
		var err error
		if createEvent.EventInfo.ScanEvent && createEvent.EventInfo.ScanSourceNode != nil {
			node = createEvent.EventInfo.ScanSourceNode
		} else {
			node, err = ev.Source.LoadNode(createEvent.EventInfo.CreateContext(ev.globalContext), createEvent.EventInfo.Path, false)
		}
		if err != nil {
			delete(batch.CreateFolders, createEvent.Key)
			if _, exists := batch.Deletes[createEvent.Key]; exists {
				delete(batch.Deletes, createEvent.Key)
			}
		} else {
			createEvent.Node = node
		}
		log.Logger(ev.globalContext).Debug("Create Folder", zap.Any("node", createEvent.Node))
	}

	detectFolderMoves(ev.globalContext, batch, ev.Target)

	var possibleMoves []*Move
	for _, deleteEvent := range batch.Deletes {
		localPath := deleteEvent.EventInfo.Path
		var dbNode *tree.Node
		if deleteEvent.Node != nil {
			// If deleteEvent has node, it is already loaded from a snapshot, no need to reload from target
			dbNode = deleteEvent.Node
		} else {
			dbNode, _ = ev.Target.LoadNode(deleteEvent.EventInfo.CreateContext(ev.globalContext), localPath)
			log.Logger(ev.globalContext).Debug("Looking for node in index", zap.Any("path", localPath), zap.Any("dbNode", dbNode))
		}
		if dbNode != nil {
			deleteEvent.Node = dbNode
			if dbNode.IsLeaf() {
				var found bool
				// Look by UUID first
				for _, createEvent := range batch.CreateFiles {
					if createEvent.Node != nil && createEvent.Node.Uuid == dbNode.Uuid {
						log.Logger(ev.globalContext).Debug("Existing leaf node with Uuid: safe move to ", createEvent.Node.ZapPath())
						createEvent.Node = dbNode
						batch.FileMoves[createEvent.Key] = createEvent
						delete(batch.Deletes, deleteEvent.Key)
						delete(batch.CreateFiles, createEvent.Key)
						found = true
						break
					}
				}
				// Look by Etag
				if !found {
					for _, createEvent := range batch.CreateFiles {
						if createEvent.Node != nil && createEvent.Node.Etag == dbNode.Etag {
							log.Logger(ev.globalContext).Debug("Existing leaf node with same ETag: enqueuing possible move", createEvent.Node.ZapPath())
							possibleMoves = append(possibleMoves, &Move{
								deleteEvent: deleteEvent,
								createEvent: createEvent,
								dbNode:      dbNode,
							})
						}
					}
				}
			}
		} else {
			_, createFileExists := batch.CreateFiles[deleteEvent.Key]
			_, createFolderExists := batch.CreateFolders[deleteEvent.Key]
			if createFileExists || createFolderExists {
				// There was a create & remove in the same batch, on a non indexed node.
				// We are not sure of the order, Stat the file.
				var testLeaf bool
				if createFileExists {
					testLeaf = true
				} else {
					testLeaf = false
				}
				existNode, _ := ev.Source.LoadNode(deleteEvent.EventInfo.CreateContext(ev.globalContext), deleteEvent.EventInfo.Path, testLeaf)
				if existNode == nil {
					// File does not exist finally, ignore totally
					if createFileExists {
						delete(batch.CreateFiles, deleteEvent.Key)
					}
					if createFolderExists {
						delete(batch.CreateFolders, deleteEvent.Key)
					}
				}
			}
			// Remove from delete anyway : node is not in the index
			delete(batch.Deletes, deleteEvent.Key)
		}
	}

	moves := sortClosestMoves(ev.globalContext, possibleMoves)
	for _, move := range moves {
		log.Logger(ev.globalContext).Debug("Picked closest move", zap.Object("move", move))
		move.createEvent.Node = move.dbNode
		batch.FileMoves[move.createEvent.Key] = move.createEvent
		delete(batch.Deletes, move.deleteEvent.Key)
		delete(batch.CreateFiles, move.createEvent.Key)
	}

	// Prune Deletes: remove children if parent is already deleted
	deleteDelete := []string{}
	for _, folderDeleteEvent := range batch.Deletes {
		deletePath := folderDeleteEvent.Node.Path
		for deleteKey, delEvent := range batch.Deletes {
			from := delEvent.Node.Path
			if len(from) > len(deletePath) && strings.HasPrefix(from, deletePath) {
				deleteDelete = append(deleteDelete, deleteKey)
			}
		}
	}
	for _, del := range deleteDelete {
		log.Logger(ev.globalContext).Debug("Ignoring Delete for key " + del + " as parent is already delete")
		delete(batch.Deletes, del)
	}

	ev.sendEvent(common.ProcessorEvent{
		Type: "filter:end",
		Data: batch,
	})
}

func (ev *EventsBatcher) ProcessEvents(events []common.EventInfo, asSession bool) {

	log.Logger(ev.globalContext).Debug("Processing Events Now", zap.Int("count", len(events)))
	batch := NewBatch()
	/*
		if p, o := common.AsSessionProvider(ev.Target); o && asSession && len(events) > 30 {
			batch.SessionProvider = p
			batch.SessionProviderContext = events[0].CreateContext(ev.globalContext)
		}
	*/

	for _, event := range events {
		log.Logger(ev.globalContext).Debug("[batcher]", zap.Any("type", event.Type), zap.Any("path", event.Path), zap.Any("sourceNode", event.ScanSourceNode))
		key := event.Path
		var bEvent = &BatchedEvent{
			Source:    ev.Source,
			Target:    ev.Target,
			Key:       key,
			EventInfo: event,
		}
		if event.Type == common.EventCreate || event.Type == common.EventRename {
			if event.Folder {
				batch.CreateFolders[key] = bEvent
			} else {
				batch.CreateFiles[key] = bEvent
			}
		} else {
			batch.Deletes[key] = bEvent
		}
	}
	ev.FilterBatch(batch)
	ev.batchOut <- batch

}

func (ev *EventsBatcher) BatchEvents(in chan common.EventInfo, out chan *Batch, duration time.Duration) {

	ev.batchOut = out
	var batch []common.EventInfo

	for {
		select {
		case event := <-in:
			//log.Logger(ev.globalContext).Info("Received S3 Event", zap.Any("e", event))
			// Add to queue
			if session := event.Metadata["X-Pydio-Session"]; session != "" {
				if strings.HasPrefix(session, "close-") {
					session = strings.TrimPrefix(session, "close-")

					ev.batchCacheMutex.Lock()
					ev.batchCache[session] = append(ev.batchCache[session], event)
					log.Logger(ev.globalContext).Debug("[batcher] Processing session")
					go ev.ProcessEvents(ev.batchCache[session], true)
					delete(ev.batchCache, session)
					ev.batchCacheMutex.Unlock()
				} else {
					ev.batchCacheMutex.Lock()
					log.Logger(ev.globalContext).Debug("[batcher] Batching Event in session "+session, zap.Any("e", event))
					ev.batchCache[session] = append(ev.batchCache[session], event)
					ev.batchCacheMutex.Unlock()
				}
			} else if event.ScanEvent || event.OperationId == "" {
				log.Logger(ev.globalContext).Debug("[batcher] Batching Event without session ", zap.Any("e", event))
				batch = append(batch, event)
			}
		case session := <-ev.closeSessionChan:
			ev.batchCacheMutex.Lock()
			if events, ok := ev.batchCache[session]; ok {
				log.Logger(ev.globalContext).Debug("[batcher] Force closing session now!")
				go ev.ProcessEvents(events, true)
				delete(ev.batchCache, session)
			}
			ev.batchCacheMutex.Unlock()
		case <-time.After(duration):
			// Process Queue
			if len(batch) > 0 {
				log.Logger(ev.globalContext).Debug("[batcher] Processing batch after timeout")
				go ev.ProcessEvents(batch, false)
				batch = nil
			}
		}

	}

}

func (ev *EventsBatcher) ForceCloseSession(sessionUuid string) {
	ev.closeSessionChan <- sessionUuid
}

func NewEventsBatcher(ctx context.Context, source common.PathSyncSource, target common.PathSyncTarget) *EventsBatcher {

	return &EventsBatcher{
		Source:           source,
		Target:           target,
		globalContext:    ctx,
		batchCache:       make(map[string][]common.EventInfo),
		batchCacheMutex:  &sync.Mutex{},
		closeSessionChan: make(chan string, 1),
	}

}
