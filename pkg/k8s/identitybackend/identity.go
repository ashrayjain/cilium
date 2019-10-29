// Copyright 2019 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package identitybackend

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/cilium/cilium/pkg/allocator"
	"github.com/cilium/cilium/pkg/idpool"
	"github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	clientset "github.com/cilium/cilium/pkg/k8s/client/clientset/versioned"
	"github.com/cilium/cilium/pkg/k8s/informer"
	"github.com/cilium/cilium/pkg/k8s/types"
	k8sversion "github.com/cilium/cilium/pkg/k8s/version"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/sirupsen/logrus"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	k8sTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

var (
	log = logging.DefaultLogger.WithField(logfields.LogSubsys, "crd-allocator")
)

func NewCRDBackend(c CRDBackendConfiguration) (allocator.Backend, error) {
	return &crdBackend{CRDBackendConfiguration: c}, nil
}

type CRDBackendConfiguration struct {
	NodeName string
	Store    cache.Store
	Client   clientset.Interface
	KeyType  allocator.AllocatorKey
}

type crdBackend struct {
	CRDBackendConfiguration
}

func (c *crdBackend) DeleteAllKeys() {
}

// sanitizeK8sLabels strips the 'k8s:' prefix in the labels generated by
// AllocatorKey.GetAsMap (when the key is k8s labels). In the CRD identity case
// we map the labels directly to the ciliumidentity CRD instance, and
// kubernetes does not allow ':' in the name of the label. These labels are not
// the canonical labels of the identity, but used to ease interaction with the
// CRD object.
func sanitizeK8sLabels(old map[string]string) (selected, skipped map[string]string) {
	k8sPrefix := labels.LabelSourceK8s + ":"
	skipped = make(map[string]string, len(old))
	selected = make(map[string]string, len(old))
	for k, v := range old {
		if !strings.HasPrefix(k, k8sPrefix) {
			skipped[k] = v
			continue // skip non-k8s labels
		}
		k = strings.TrimPrefix(k, k8sPrefix) // k8s: is redundant
		selected[k] = v
	}
	return selected, skipped
}

// AllocateID will create an identity CRD, thus creating the identity for this
// key-> ID mapping.
// Note: This does not create a reference to this node to indicate that it is
// using this identity. That must be done with AcquireReference.
// Note: the lock field is not supported with the k8s CRD allocator.
func (c *crdBackend) AllocateID(ctx context.Context, id idpool.ID, key allocator.AllocatorKey) error {
	selectedLabels, skippedLabels := sanitizeK8sLabels(key.GetAsMap())
	log.WithField(logfields.Labels, skippedLabels).Info("Skipped non-kubernetes labels when labelling ciliumidentity. All labels will still be used in identity determination")

	identity := &v2.CiliumIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:   id.String(),
			Labels: selectedLabels,
		},
		SecurityLabels: key.GetAsMap(),
		Status: v2.IdentityStatus{
			Nodes: map[string]metav1.Time{
				c.NodeName: metav1.Now(),
			},
		},
	}

	_, err := c.Client.CiliumV2().CiliumIdentities().Create(identity)
	return err
}

func (c *crdBackend) AllocateIDIfLocked(ctx context.Context, id idpool.ID, key allocator.AllocatorKey, lock kvstore.KVLocker) error {
	return c.AllocateID(ctx, id, key)
}

// JSONPatch structure based on the RFC 6902
// Note: This mirros pkg/k8s/json_patch.go but using that directly would cause
// an import loop.
type JSONPatch struct {
	OP    string      `json:"op,omitempty"`
	Path  string      `json:"path,omitempty"`
	Value interface{} `json:"value"`
}

// AcquireReference updates the status field of the CRD corresponding to id
// with this node. This marks that CRD as used by this node, and will stop it
// being garbage collected.
// Note: the lock field is not supported with the k8s CRD allocator.
func (c *crdBackend) AcquireReference(ctx context.Context, id idpool.ID, key allocator.AllocatorKey, lock kvstore.KVLocker) error {
	identity := c.get(ctx, key)
	if identity == nil {
		return fmt.Errorf("identity does not exist")
	}

	capabilities := k8sversion.Capabilities()
	identityOps := c.Client.CiliumV2().CiliumIdentities()

	var err error
	if capabilities.Patch {
		var patch []byte
		patch, err = json.Marshal([]JSONPatch{
			{
				OP:    "test",
				Path:  "/status",
				Value: nil,
			},
			{
				OP:   "add",
				Path: "/status",
				Value: v2.IdentityStatus{
					Nodes: map[string]metav1.Time{
						c.NodeName: metav1.Now(),
					},
				},
			},
		})
		if err != nil {
			return err
		}

		_, err = identityOps.Patch(identity.GetName(), k8sTypes.JSONPatchType, patch, "status")
		if err != nil {
			patch, err = json.Marshal([]JSONPatch{
				{
					OP:    "replace",
					Path:  "/status/nodes/" + c.NodeName,
					Value: metav1.Now(),
				},
			})
			if err != nil {
				return err
			}
			_, err = identityOps.Patch(identity.GetName(), k8sTypes.JSONPatchType, patch, "status")
		}

		if err == nil {
			return nil
		}
		log.WithError(err).Debug("Error patching status. Continuing update via UpdateStatus")
		/* fall through and attempt UpdateStatus() or Update() */
	}

	identityCopy := identity.DeepCopy()
	if identityCopy.Status.Nodes == nil {
		identityCopy.Status.Nodes = map[string]metav1.Time{
			c.NodeName: metav1.Now(),
		}
	} else {
		identityCopy.Status.Nodes[c.NodeName] = metav1.Now()
	}

	if capabilities.UpdateStatus {
		_, err = identityOps.UpdateStatus(identityCopy.CiliumIdentity)
		if err == nil {
			return nil
		}
		log.WithError(err).Debug("Error updating status. Continuing update via Update")
		/* fall through and attempt Update() */
	}

	_, err = identityOps.Update(identityCopy.CiliumIdentity)
	return err
}

func (c *crdBackend) RunGC(ctx context.Context, staleKeysPrevRound map[string]uint64) (map[string]uint64, error) {
	return nil, nil
}

// UpdateKey refreshes the reference that this node is using this key->ID
// mapping. It assumes that the identity already exists but will recreate it if
// reliablyMissing is true.
// Note: the lock field is not supported with the k8s CRD allocator.
func (c *crdBackend) UpdateKey(ctx context.Context, id idpool.ID, key allocator.AllocatorKey, reliablyMissing bool) error {
	var err error

	if err := c.AcquireReference(ctx, id, key, nil); err == nil {
		log.WithError(err).WithFields(logrus.Fields{
			logfields.Identity: id,
			logfields.Labels:   key,
		}).Debug("Acquired reference for identity")
		return nil
	}

	// The CRD (aka the master key) is missing. Try to recover by recreating it
	// if reliablyMissing is set.
	log.WithFields(logrus.Fields{
		logfields.Identity: id,
		logfields.Labels:   key,
	}).Warning("Unable update CRD identity information with a reference for this node")

	if reliablyMissing {
		// Recreate a missing master key
		if err = c.AllocateID(ctx, id, key); err != nil {
			return fmt.Errorf("Unable recreate missing CRD identity %q->%q: %s", key, id, err)
		}
	}

	return nil
}

func (c *crdBackend) UpdateKeyIfLocked(ctx context.Context, id idpool.ID, key allocator.AllocatorKey, reliablyMissing bool, lock kvstore.KVLocker) error {
	return c.UpdateKey(ctx, id, key, reliablyMissing)
}

// Lock does not return a lock object. Locking is not supported with the k8s
// CRD allocator. It is here to meet interface requirements.
func (c *crdBackend) Lock(ctx context.Context, key allocator.AllocatorKey) (kvstore.KVLocker, error) {
	return &crdLock{}, nil
}

type crdLock struct{}

// Unlock does not unlock a lock object. Locking is not supported with the k8s
// CRD allocator. It is here to meet interface requirements.
func (c *crdLock) Unlock(ctx context.Context) error {
	return nil
}

// Comparator does nothing. Locking is not supported with the k8s
// CRD allocator. It is here to meet interface requirements.
func (c *crdLock) Comparator() interface{} {
	return nil
}

func (c *crdBackend) get(ctx context.Context, key allocator.AllocatorKey) *types.Identity {
	if c.Store == nil {
		return nil
	}

	for _, identityObject := range c.Store.List() {
		identity, ok := identityObject.(*types.Identity)
		if !ok {
			return nil
		}

		if reflect.DeepEqual(identity.SecurityLabels, key.GetAsMap()) {
			return identity
		}
	}

	return nil
}

// Get returns the ID which is allocated to a key in the identity CRDs in
// kubernetes.
// Note: the lock field is not supported with the k8s CRD allocator.
func (c *crdBackend) Get(ctx context.Context, key allocator.AllocatorKey) (idpool.ID, error) {
	identity := c.get(ctx, key)
	if identity == nil {
		return idpool.NoID, nil
	}

	id, err := strconv.ParseUint(identity.Name, 10, 64)
	if err != nil {
		return idpool.NoID, fmt.Errorf("unable to parse value '%s': %s", identity.Name, err)
	}

	return idpool.ID(id), nil
}

func (c *crdBackend) GetIfLocked(ctx context.Context, key allocator.AllocatorKey, lock kvstore.KVLocker) (idpool.ID, error) {
	return c.Get(ctx, key)
}

// GetByID returns the key associated with an ID. Returns nil if no key is
// associated with the ID.
// Note: the lock field is not supported with the k8s CRD allocator.
func (c *crdBackend) GetByID(ctx context.Context, id idpool.ID) (allocator.AllocatorKey, error) {
	if c.Store == nil {
		return nil, fmt.Errorf("store is not available yet")
	}

	identityTemplate := &types.Identity{
		CiliumIdentity: &v2.CiliumIdentity{
			ObjectMeta: metav1.ObjectMeta{
				Name: id.String(),
			},
		},
	}

	obj, exists, err := c.Store.Get(identityTemplate)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	identity, ok := obj.(*types.Identity)
	if !ok {
		return nil, fmt.Errorf("invalid object")
	}

	return c.KeyType.PutKeyFromMap(identity.SecurityLabels), nil
}

// Release dissociates this node from using the identity bound to key. When an
// identity has no references it may be garbage collected.
func (c *crdBackend) Release(ctx context.Context, key allocator.AllocatorKey) (err error) {
	identity := c.get(ctx, key)
	if identity == nil {
		return fmt.Errorf("unable to release identity %s: identity does not exist", key)
	}

	if _, ok := identity.Status.Nodes[c.NodeName]; !ok {
		return fmt.Errorf("unable to release identity %s: identity is unused", key)
	}

	delete(identity.Status.Nodes, c.NodeName)

	capabilities := k8sversion.Capabilities()

	identityOps := c.Client.CiliumV2().CiliumIdentities()
	if capabilities.Patch {
		var patch []byte
		patch, err = json.Marshal([]JSONPatch{
			{
				OP:   "delete",
				Path: "/status/nodes/" + c.NodeName,
			},
		})
		if err != nil {
			return err
		}
		_, err = identityOps.Patch(identity.GetName(), k8sTypes.JSONPatchType, patch, "status")
		if err == nil {
			return nil
		}
		log.WithError(err).Debug("Error patching status. Continuing update via UpdateStatus")
		/* fall through and attempt UpdateStatus() or Update() */
	}

	identityCopy := identity.DeepCopy()
	if identityCopy.Status.Nodes == nil {
		return nil
	}

	if capabilities.UpdateStatus {
		_, err = identityOps.UpdateStatus(identityCopy.CiliumIdentity)
		if err == nil {
			return nil
		}
		log.WithError(err).Debug("Error updating status. Continuing update via Update")
		/* fall through and attempt Update() */
	}

	_, err = identityOps.Update(identityCopy.CiliumIdentity)
	return err
}

func (c *crdBackend) ListAndWatch(handler allocator.CacheMutations, stopChan chan struct{}) {
	c.Store = cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
	identityInformer := informer.NewInformerWithStore(
		cache.NewListWatchFromClient(c.Client.CiliumV2().RESTClient(),
			"ciliumidentities", v1.NamespaceAll, fields.Everything()),
		&v2.CiliumIdentity{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if identity, ok := obj.(*types.Identity); ok {
					if id, err := strconv.ParseUint(identity.Name, 10, 64); err == nil {
						handler.OnAdd(idpool.ID(id), c.KeyType.PutKeyFromMap(identity.SecurityLabels))
					}
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if identity, ok := newObj.(*types.Identity); ok {
					if id, err := strconv.ParseUint(identity.Name, 10, 64); err == nil {
						handler.OnModify(idpool.ID(id), c.KeyType.PutKeyFromMap(identity.SecurityLabels))
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				// The delete event is sometimes for items with unknown state that are
				// deleted anyway.
				if deleteObj, isDeleteObj := obj.(cache.DeletedFinalStateUnknown); isDeleteObj {
					obj = deleteObj.Obj
				}

				if identity, ok := obj.(*types.Identity); ok {
					if id, err := strconv.ParseUint(identity.Name, 10, 64); err == nil {
						handler.OnDelete(idpool.ID(id), c.KeyType.PutKeyFromMap(identity.SecurityLabels))
					}
				} else {
					log.Debugf("Ignoring unknown delete event %#v", obj)
				}
			},
		},
		types.ConvertToIdentity,
		c.Store,
	)

	go func() {
		if ok := cache.WaitForCacheSync(stopChan, identityInformer.HasSynced); ok {
			handler.OnListDone()
		}
	}()

	identityInformer.Run(stopChan)
}

func (c *crdBackend) Status() (string, error) {
	return "OK", nil
}

func (c *crdBackend) Encode(v string) string {
	return v
}
