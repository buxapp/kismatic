package controller

import (
	"fmt"
	"log"

	"github.com/apprenda/kismatic/pkg/install"
	"github.com/apprenda/kismatic/pkg/provision"
	"github.com/apprenda/kismatic/pkg/store"
	"github.com/google/go-cmp/cmp"
)

const (
	planning        = "planning"
	planningFailed  = "planningFailed"
	planned         = "planned"
	provisioning    = "provisioning"
	provisionFailed = "provisionFailed"
	provisioned     = "provisioned"
	installing      = "installing"
	installFailed   = "installFailed"
	installed       = "installed"
	modifying       = "modifying"
	modifyFailed    = "modifyFailed"
	destroying      = "destroying"
	destroyFailed   = "destroyFailed"
	destroyed       = "destroyed"
)

// The clusterController manages the lifecycle of a single cluster.
type clusterController struct {
	clusterName string
	clusterSpec store.ClusterSpec
	// TODO: The plan is only stored in memory. If the controller goes down, it
	// will be lost.
	installPlan    install.Plan
	log            *log.Logger
	executor       install.Executor
	newProvisioner func(store.Cluster) provision.Provisioner
	clusterStore   store.ClusterStore
}

// This is the controller's reconciliation loop. It listens on a channel for
// changes to the cluster spec. In the case of a mismatch between the current
// state and the desired state, the controller will take action by transitioning
// the cluster towards the desired state.
func (c *clusterController) run(watch <-chan struct{}) {
	c.log.Printf("started controller for cluster %q", c.clusterName)
	for _ = range watch {
		cluster, err := c.clusterStore.Get(c.clusterName)
		if err != nil {
			c.log.Printf("error getting cluster from store: %v", err)
			continue
		}
		c.log.Printf("cluster %q - current state: %s, desired state: %s, waiting for retry: %v", c.clusterName, cluster.Status.CurrentState, cluster.Spec.DesiredState, cluster.Status.WaitingForManualRetry)

		// If the cluster spec has changed and we are not trying to destroy, we need to plan again
		if !cmp.Equal(cluster.Spec, c.clusterSpec) && cluster.Spec.DesiredState != destroyed {
			cluster.Status.CurrentState = planning
		}

		// If we have reached the desired state or we are waiting for a manual
		// retry, don't do anything
		if cluster.Status.CurrentState == cluster.Spec.DesiredState || cluster.Status.WaitingForManualRetry {
			continue
		}

		// Transition the cluster to the next state
		transitionedCluster := c.transition(*cluster)

		// Transitions are long - O(minutes). Get the latest cluster spec from
		// the store before updating it.
		// TODO: Ideally we would run this in a transaction, but the current
		// implementation of the store does not expose txs.
		cluster, err = c.clusterStore.Get(c.clusterName)
		if err != nil {
			c.log.Printf("error getting cluster from store: %v", err)
			continue
		}

		// Update the cluster status with the latest
		cluster.Status = transitionedCluster.Status
		err = c.clusterStore.Put(c.clusterName, *cluster)
		if err != nil {
			c.log.Printf("error storing cluster state: %v. The cluster's current state is %q and desired state is %q", err, cluster.Status.CurrentState, cluster.Spec.DesiredState)
			continue
		}

		// Update the controller's state of the world to the latest state.
		c.clusterSpec = cluster.Spec

		// If the cluster has been destroyed, remove the cluster from the store
		// and stop the controller
		if cluster.Status.CurrentState == destroyed {
			err := c.clusterStore.Delete(c.clusterName)
			if err != nil {
				// At this point, the cluster has already been destroyed, but we
				// failed to remove the cluster resource from the database. The
				// only thing that can be done is for the user to issue another
				// delete so that we try again.
				c.log.Printf("could not delete cluster %q from store: %v", c.clusterName, err)
				continue
			}
			c.log.Printf("cluster %q has been destroyed. stoppping controller.", c.clusterName)
			return
		}
	}
	c.log.Printf("stopping controller that was managing cluster %q", c.clusterName)
}

// transition performs an action to take the cluster to the next state. The
// action to be performed depends on the current state and the desired state.
// Once the action is done, an updated cluster spec is returned that reflects
// the outcome of the action.
func (c *clusterController) transition(cluster store.Cluster) store.Cluster {
	if cluster.Spec.DesiredState == cluster.Status.CurrentState {
		return cluster
	}
	// Figure out where to go from the current state
	switch cluster.Status.CurrentState {
	case "": // This is the initial state
		cluster.Status.CurrentState = planning
		return cluster
	case planning:
		return c.plan(cluster)
	case planned:
		cluster.Status.CurrentState = provisioning
		return cluster
	case planningFailed:
		if cluster.Spec.DesiredState == destroyed {
			cluster.Status.CurrentState = destroying
			return cluster
		}
		cluster.Status.CurrentState = planning
		return cluster
	case provisioning:
		return c.provision(cluster)
	case provisioned:
		if cluster.Spec.DesiredState == destroyed {
			cluster.Status.CurrentState = destroying
			return cluster
		}
		cluster.Status.CurrentState = installing
		return cluster
	case provisionFailed:
		if cluster.Spec.DesiredState == destroyed {
			cluster.Status.CurrentState = destroying
			return cluster
		}
		cluster.Status.CurrentState = provisioning
		return cluster
	case destroying:
		return c.destroy(cluster)
	case installing:
		return c.install(cluster)
	case installFailed:
		if cluster.Spec.DesiredState == destroyed {
			cluster.Status.CurrentState = destroying
			return cluster
		}
		cluster.Status.CurrentState = installing
		return cluster
	case installed:
		if cluster.Spec.DesiredState == destroyed {
			cluster.Status.CurrentState = destroying
			return cluster
		}
		c.log.Printf("cluster %q: cannot transition to %q from the 'installed' state", c.clusterName, cluster.Spec.DesiredState)
		cluster.Status.WaitingForManualRetry = true
		return cluster
	default:
		// Log a message, and set WaitingForManualRetry to true so that we don't get
		// stuck in an infinte loop. The only thing the user can do in this case
		// is delete the cluster and file a bug, as this scenario should not
		// happen.
		c.log.Printf("cluster %q: the desired state is %q, but there is no transition defined for the cluster's current state %q", c.clusterName, cluster.Spec.DesiredState, cluster.Status.CurrentState)
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}
}

func (c *clusterController) plan(cluster store.Cluster) store.Cluster {
	c.log.Printf("planning installation for cluster %q", c.clusterName)
	plan, err := buildPlan(c.clusterName, cluster.Spec, c.installPlan.Cluster.AdminPassword)
	if err != nil {
		c.log.Printf("error planning installation for cluster %q: %v", c.clusterName, err)
		cluster.Status.CurrentState = planningFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}
	c.installPlan = *plan
	cluster.Status.CurrentState = planned
	return cluster
}

func (c *clusterController) provision(cluster store.Cluster) store.Cluster {
	c.log.Printf("provisioning infrastructure for cluster %q", c.clusterName)
	provisioner := c.newProvisioner(cluster)
	updatedPlan, err := provisioner.Provision(c.installPlan)
	if err != nil {
		c.log.Printf("error provisioning infrastructure for cluster %q: %v", c.clusterName, err)
		cluster.Status.CurrentState = provisionFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}
	c.installPlan = *updatedPlan
	cluster.Status.CurrentState = provisioned
	cluster.Status.ClusterIP = updatedPlan.Master.LoadBalancedFQDN
	return cluster
}

func (c *clusterController) destroy(cluster store.Cluster) store.Cluster {
	c.log.Printf("destroying cluster %q", c.clusterName)
	provisioner := c.newProvisioner(cluster)
	err := provisioner.Destroy(c.clusterName)
	if err != nil {
		c.log.Printf("error destroying cluster %q: %v", c.clusterName, err)
		cluster.Status.CurrentState = destroyFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}
	cluster.Status.CurrentState = destroyed
	return cluster
}

func (c *clusterController) install(cluster store.Cluster) store.Cluster {
	c.log.Printf("installing cluster %q", c.clusterName)
	plan := c.installPlan

	err := c.executor.RunPreFlightCheck(&plan)
	if err != nil {
		c.log.Printf("cluster %q: error running preflight checks: %v", c.clusterName, err)
		cluster.Status.CurrentState = installFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}

	err = c.executor.GenerateCertificates(&plan, false)
	if err != nil {
		c.log.Printf("cluster %q: error generating certificates: %v", c.clusterName, err)
		cluster.Status.CurrentState = installFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}

	err = c.executor.GenerateKubeconfig(plan)
	if err != nil {
		c.log.Printf("cluster %q: error generating kubeconfig file: %v", c.clusterName, err)
		cluster.Status.CurrentState = installFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}

	err = c.executor.Install(&plan, true)
	if err != nil {
		c.log.Printf("cluster %q: error installing the cluster: %v", c.clusterName, err)
		cluster.Status.CurrentState = installFailed
		cluster.Status.WaitingForManualRetry = true
		return cluster
	}

	// Skip the smoketest if the user asked us to skip the installation of a
	// networking stack
	if !plan.NetworkConfigured() {
		cluster.Status.CurrentState = installed
		return cluster
	}

	err = c.executor.RunSmokeTest(&plan)
	if err != nil {
		c.log.Printf("cluster %q: error running smoke test against the cluster: %v", c.clusterName, err)
		cluster.Status.CurrentState = installFailed
		return cluster
	}

	cluster.Status.CurrentState = installed
	return cluster
}

func buildPlan(name string, clusterSpec store.ClusterSpec, existingPassword string) (*install.Plan, error) {
	// Build the plan template
	planTemplate := install.PlanTemplateOptions{
		AdminPassword: existingPassword,
		EtcdNodes:     clusterSpec.EtcdCount,
		MasterNodes:   clusterSpec.MasterCount,
		WorkerNodes:   clusterSpec.WorkerCount,
		IngressNodes:  clusterSpec.IngressCount,
	}
	planner := &install.BytesPlanner{}
	if err := install.WritePlanTemplate(planTemplate, planner); err != nil {
		return nil, fmt.Errorf("could not decode request body: %v", err)
	}
	var p *install.Plan
	p, err := planner.Read()
	if err != nil {
		return nil, fmt.Errorf("could not read plan: %v", err)
	}
	// Set values in the plan
	p.Cluster.Name = name
	p.Provisioner = install.Provisioner{Provider: clusterSpec.Provisioner.Provider}

	// Set values that depend on the cloud where the cluster will run
	// TODO: Handle provisioner specific options (e.g. AWS Region)
	switch clusterSpec.Provisioner.Provider {
	case "aws":
		p.Provisioner.AWSOptions = &install.AWSProvisionerOptions{}
		// nothing
	case "azure":
		p.AddOns.CNI.Provider = "weave"
	}
	return p, nil
}
