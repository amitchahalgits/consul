package gateways

import (
	"fmt"
	"time"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/consul/state"
	"github.com/hashicorp/consul/agent/structs"
)

// gatewayMeta embeds both a BoundAPIGateway and its corresponding APIGateway.
// This is used when binding routes to a gateway to ensure that a route's protocol (e.g. http)
// matches the protocol of the listener it wants to bind to. The binding modifies the
// "bound" gateway, but relies on the "gateway" to determine the protocol of the listener.
type gatewayMeta struct {
	// BoundGateway is the bound-api-gateway config entry for a given gateway.
	BoundGateway *structs.BoundAPIGatewayConfigEntry
	// Gateway is the api-gateway config entry for the gateway.
	Gateway *structs.APIGatewayConfigEntry
}

// getAllGatewayMeta returns a pre-constructed list of all valid gateway and state
// tuples based on the state coming from the store. Any gateway that does not have
// a current bound state will be filtered out.
func getAllGatewayMeta(store *state.Store) ([]*gatewayMeta, error) {
	_, gateways, err := store.ConfigEntriesByKind(nil, structs.APIGateway, acl.WildcardEnterpriseMeta())
	if err != nil {
		return nil, err
	}
	_, boundGateways, err := store.ConfigEntriesByKind(nil, structs.BoundAPIGateway, acl.WildcardEnterpriseMeta())
	if err != nil {
		return nil, err
	}

	meta := make([]*gatewayMeta, 0, len(boundGateways))
	for _, b := range boundGateways {
		bound := b.(*structs.BoundAPIGatewayConfigEntry)
		for _, g := range gateways {
			gateway := g.(*structs.APIGatewayConfigEntry)
			if bound.References(gateway) {
				meta = append(meta, &gatewayMeta{
					BoundGateway: bound,
					Gateway:      gateway,
				})
				break
			}
		}
	}
	return meta, nil
}

// updateRouteBinding takes a parent resource reference and a BoundRoute and
// modifies the listeners on the BoundAPIGateway config entry in GatewayMeta
// to reflect the binding of the route to the gateway.
//
// If the reference is not valid or the route's protocol does not match the
// targeted listener's protocol, a mapping of parent references to associated
// errors is returned.
func (g *gatewayMeta) updateRouteBinding(refs []structs.ResourceReference, route structs.BoundRoute) (bool, map[structs.ResourceReference]error) {
	if g.BoundGateway == nil || g.Gateway == nil {
		return false, nil
	}

	didUpdate := false
	errors := make(map[structs.ResourceReference]error)

	if len(g.BoundGateway.Listeners) == 0 {
		for _, ref := range refs {
			errors[ref] = fmt.Errorf("route cannot bind because gateway has no listeners")
		}
		return false, errors
	}

	for i, listener := range g.BoundGateway.Listeners {
		routeRef := structs.ResourceReference{
			Kind:           route.GetKind(),
			Name:           route.GetName(),
			EnterpriseMeta: *route.GetEnterpriseMeta(),
		}
		// Unbind to handle any stale route references.
		didUnbind := listener.UnbindRoute(routeRef)
		if didUnbind {
			didUpdate = true
		}
		g.BoundGateway.Listeners[i] = listener

		for _, ref := range refs {
			didBind, err := g.bindRoute(ref, route)
			if err != nil {
				errors[ref] = err
			}
			if didBind {
				didUpdate = true
			}
		}
	}

	return didUpdate, errors
}

// bindRoute takes a parent reference and a route and attempts to bind the route to the
// bound gateway in the gatewayMeta struct. It returns true if the route was bound and
// false if it was not. If the route fails to bind, an error is returned.
//
// Binding logic binds a route to one or more listeners on the Bound gateway.
// For a route to successfully bind it must:
//   - have a parent reference to the gateway
//   - have a parent reference with a section name matching the name of a listener
//     on the gateway. If the section name is `""`, the route will be bound to all
//     listeners on the gateway whose protocol matches the route's protocol.
//   - have a protocol that matches the protocol of the listener it is being bound to.
func (g *gatewayMeta) bindRoute(ref structs.ResourceReference, route structs.BoundRoute) (bool, error) {
	if g.BoundGateway == nil || g.Gateway == nil {
		return false, fmt.Errorf("gateway cannot be found")
	}

	if ref.Kind != structs.APIGateway || g.Gateway.Name != ref.Name || !g.Gateway.EnterpriseMeta.IsSame(&ref.EnterpriseMeta) {
		return false, nil
	}

	if len(g.BoundGateway.Listeners) == 0 {
		return false, fmt.Errorf("route cannot bind because gateway has no listeners")
	}

	didBind := false
	for _, listener := range g.Gateway.Listeners {
		// A route with a section name of "" is bound to all listeners on the gateway.
		if listener.Name != ref.SectionName && ref.SectionName != "" {
			continue
		}

		if listener.Protocol == route.GetProtocol() {
			routeRef := structs.ResourceReference{
				Kind:           route.GetKind(),
				Name:           route.GetName(),
				EnterpriseMeta: *route.GetEnterpriseMeta(),
			}
			i, boundListener := g.boundListenerByName(listener.Name)
			if boundListener != nil && boundListener.BindRoute(routeRef) {
				didBind = true
				g.BoundGateway.Listeners[i] = *boundListener
			}
		} else if ref.SectionName != "" {
			// Failure to bind to a specific listener is an error
			return false, fmt.Errorf("failed to bind route %s to gateway %s: listener %s is not a %s listener", route.GetName(), g.Gateway.Name, listener.Name, route.GetProtocol())
		}
	}

	if !didBind {
		return didBind, fmt.Errorf("failed to bind route %s to gateway %s: no valid listener has name '%s' and uses %s protocol", route.GetName(), g.Gateway.Name, ref.SectionName, route.GetProtocol())
	}

	return didBind, nil
}

// unbindRoute takes a route and unbinds it from all of the listeners on a gateway.
// It returns true if the route was unbound and false if it was not.
func (g *gatewayMeta) unbindRoute(route structs.ResourceReference) bool {
	if g.BoundGateway == nil {
		return false
	}

	didUnbind := false
	for i, listener := range g.BoundGateway.Listeners {
		if listener.UnbindRoute(route) {
			didUnbind = true
			g.BoundGateway.Listeners[i] = listener
		}
	}

	return didUnbind
}

func (g *gatewayMeta) boundListenerByName(name string) (int, *structs.BoundAPIGatewayListener) {
	for i, listener := range g.BoundGateway.Listeners {
		if listener.Name == name {
			return i, &listener
		}
	}
	return -1, nil
}

func (g *gatewayMeta) checkConflicts() (structs.ControlledConfigEntry, bool) {
	now := time.Now()
	updater := structs.NewStatusUpdater(g.Gateway)
	for i, listener := range g.BoundGateway.Listeners {
		protocol := g.Gateway.Listeners[i].Protocol
		switch protocol {
		case structs.ListenerProtocolTCP:
			if len(listener.Routes) > 1 {
				updater.SetCondition(structs.Condition{
					Type:               "Conflicted",
					Status:             "True",
					Reason:             "RouteConflict",
					Message:            "TCP-based listeners currently only support binding a single route",
					LastTransitionTime: &now,
				})
			}
		}
	}

	toUpdate, shouldUpdate := updater.UpdateEntry()
	return toUpdate, shouldUpdate
}
