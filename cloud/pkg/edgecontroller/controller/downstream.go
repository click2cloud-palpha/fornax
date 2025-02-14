package controller

import (
	"context"
	"reflect"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgov1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	beehiveContext "github.com/kubeedge/beehive/pkg/core/context"
	"github.com/kubeedge/beehive/pkg/core/model"
	edgeclustersv1 "github.com/kubeedge/kubeedge/cloud/pkg/apis/edgeclusters/v1"
	routerv1 "github.com/kubeedge/kubeedge/cloud/pkg/apis/rules/v1"
	crdClientset "github.com/kubeedge/kubeedge/cloud/pkg/client/clientset/versioned"
	crdinformers "github.com/kubeedge/kubeedge/cloud/pkg/client/informers/externalversions"
	crdlister "github.com/kubeedge/kubeedge/cloud/pkg/client/listers/edgeclusters/v1"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/client"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/informers"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/modules"
	"github.com/kubeedge/kubeedge/cloud/pkg/edgecontroller/constants"
	"github.com/kubeedge/kubeedge/cloud/pkg/edgecontroller/manager"
	"github.com/kubeedge/kubeedge/cloud/pkg/edgecontroller/messagelayer"
)

// DownstreamController watch kubernetes api server and send change to edge
type DownstreamController struct {
	kubeClient kubernetes.Interface

	crdClient crdClientset.Interface

	messageLayer messagelayer.MessageLayer

	podManager *manager.PodManager

	configmapManager *manager.ConfigMapManager

	secretManager *manager.SecretManager

	nodeManager *manager.NodesManager

	serviceManager *manager.ServiceManager

	endpointsManager *manager.EndpointsManager

	rulesManager *manager.RuleManager

	ruleEndpointsManager *manager.RuleEndpointManager

	missionsManager *manager.MissionManager

	edgeClusterManager *manager.EdgeClusterManager

	lc *manager.LocationCache

	svcLister clientgov1.ServiceLister

	podLister clientgov1.PodLister

	missionLister crdlister.MissionLister
}

func (dc *DownstreamController) syncPod() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncPod loop")
			return
		case e := <-dc.podManager.Events():
			pod, ok := e.Object.(*v1.Pod)
			if !ok {
				klog.Warningf("object type: %T unsupported", e.Object)
				continue
			}
			if !dc.lc.IsEdgeNode(pod.Spec.NodeName) {
				continue
			}
			resource, err := messagelayer.BuildResource(pod.Spec.NodeName, pod.Namespace, model.ResourceTypePod, pod.Name)
			if err != nil {
				klog.Warningf("built message resource failed with error: %s", err)
				continue
			}
			msg := model.NewMessage("").
				SetResourceVersion(pod.ResourceVersion).
				FillBody(pod)
			switch e.Type {
			case watch.Added:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.InsertOperation)
				dc.lc.AddOrUpdatePod(*pod)
			case watch.Deleted:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.DeleteOperation)
			case watch.Modified:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.UpdateOperation)
				dc.lc.AddOrUpdatePod(*pod)
			default:
				klog.Warningf("pod event type: %s unsupported", e.Type)
				continue
			}

			dc.SendMessage(msg)
		}
	}
}

func (dc *DownstreamController) syncConfigMap() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncConfigMap loop")
			return
		case e := <-dc.configmapManager.Events():
			configMap, ok := e.Object.(*v1.ConfigMap)
			if !ok {
				klog.Warningf("object type: %T unsupported", e.Object)
				continue
			}
			var operation string
			switch e.Type {
			case watch.Added:
				operation = model.InsertOperation
			case watch.Modified:
				operation = model.UpdateOperation
			case watch.Deleted:
				operation = model.DeleteOperation
			default:
				// unsupported operation, no need to send to any node
				klog.Warningf("config map event type: %s unsupported", e.Type)
				continue // continue to next select
			}

			nodes := dc.lc.ConfigMapNodes(configMap.Namespace, configMap.Name)
			if e.Type == watch.Deleted {
				dc.lc.DeleteConfigMap(configMap.Namespace, configMap.Name)
			}
			klog.V(4).Infof("there are %d nodes need to sync config map, operation: %s", len(nodes), e.Type)
			for _, n := range nodes {
				resource, err := messagelayer.BuildResource(n, configMap.Namespace, model.ResourceTypeConfigmap, configMap.Name)
				if err != nil {
					klog.Warningf("build message resource failed with error: %s", err)
					continue
				}
				msg := model.NewMessage("").
					SetResourceVersion(configMap.ResourceVersion).
					BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, operation).
					FillBody(configMap)

				dc.SendMessage(msg)
			}
		}
	}
}

func (dc *DownstreamController) syncSecret() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncSecret loop")
			return
		case e := <-dc.secretManager.Events():
			secret, ok := e.Object.(*v1.Secret)
			if !ok {
				klog.Warningf("object type: %T unsupported", e.Object)
				continue
			}
			var operation string
			switch e.Type {
			case watch.Added:
				// TODO: rollback when all edge upgrade to 2.1.6 or upper
				fallthrough
			case watch.Modified:
				operation = model.UpdateOperation
			case watch.Deleted:
				operation = model.DeleteOperation
			default:
				// unsupported operation, no need to send to any node
				klog.Warningf("secret event type: %s unsupported", e.Type)
				continue // continue to next select
			}

			nodes := dc.lc.SecretNodes(secret.Namespace, secret.Name)
			if e.Type == watch.Deleted {
				dc.lc.DeleteSecret(secret.Namespace, secret.Name)
			}
			klog.V(4).Infof("there are %d nodes need to sync secret, operation: %s", len(nodes), e.Type)
			for _, n := range nodes {
				resource, err := messagelayer.BuildResource(n, secret.Namespace, model.ResourceTypeSecret, secret.Name)
				if err != nil {
					klog.Warningf("build message resource failed with error: %s", err)
					continue
				}
				msg := model.NewMessage("").
					SetResourceVersion(secret.ResourceVersion).
					BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, operation).
					FillBody(secret)

				dc.SendMessage(msg)
			}
		}
	}
}

func (dc *DownstreamController) syncEdgeNodes() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncEdgeNodes loop")
			return
		case e := <-dc.nodeManager.Events():
			node, ok := e.Object.(*v1.Node)
			if !ok {
				klog.Warningf("Object type: %T unsupported", e.Object)
				continue
			}
			switch e.Type {
			case watch.Added:
				fallthrough
			case watch.Modified:
				// When node comes to running, send all the service/endpoints/pods information to edge
				for _, nsc := range node.Status.Conditions {
					if nsc.Type != v1.NodeReady {
						continue
					}
					nstatus := string(nsc.Status)
					status, _ := dc.lc.GetNodeStatus(node.ObjectMeta.Name)
					dc.lc.UpdateEdgeNode(node.ObjectMeta.Name, nstatus)
					if nsc.Status != v1.ConditionTrue || status == nstatus {
						continue
					}
				}
			case watch.Deleted:
				dc.lc.DeleteNode(node.ObjectMeta.Name)

				resource, err := messagelayer.BuildResource(node.Name, "namespace", constants.ResourceNode, node.Name)
				if err != nil {
					klog.Warningf("Built message resource failed with error: %s", err)
					break
				}
				msg := model.NewMessage("").
					BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.DeleteOperation)
				dc.SendMessage(msg)
			default:
				// unsupported operation, no need to send to any node
				klog.Warningf("Node event type: %s unsupported", e.Type)
			}
		}
	}
}

func (dc *DownstreamController) syncRule() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncRule loop")
			return
		case e := <-dc.rulesManager.Events():
			klog.V(4).Infof("Get rule events: event type: %s.", e.Type)
			rule, ok := e.Object.(*routerv1.Rule)
			if !ok {
				klog.Warningf("object type: %T unsupported", e.Object)
				continue
			}
			klog.V(4).Infof("Get rule events: rule object: %+v.", rule)

			resource, err := messagelayer.BuildResourceForRouter(model.ResourceTypeRule, rule.Name)
			if err != nil {
				klog.Warningf("built message resource failed with error: %s", err)
				continue
			}
			msg := model.NewMessage("").
				SetResourceVersion(rule.ResourceVersion).
				FillBody(rule)
			switch e.Type {
			case watch.Added:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.InsertOperation)
			case watch.Deleted:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.DeleteOperation)
			case watch.Modified:
				klog.Warningf("rule event type: %s unsupported", e.Type)
				continue
			default:
				klog.Warningf("rule event type: %s unsupported", e.Type)
				continue
			}
			dc.SendMessage(msg)
		}
	}
}

func (dc *DownstreamController) syncRuleEndpoint() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncRuleEndpoint loop")
			return
		case e := <-dc.ruleEndpointsManager.Events():
			klog.V(4).Infof("Get ruleEndpoint events: event type: %s.", e.Type)
			ruleEndpoint, ok := e.Object.(*routerv1.RuleEndpoint)
			if !ok {
				klog.Warningf("object type: %T unsupported", e.Object)
				continue
			}
			klog.V(4).Infof("Get ruleEndpoint events: ruleEndpoint object: %+v.", ruleEndpoint)

			resource, err := messagelayer.BuildResourceForRouter(model.ResourceTypeRuleEndpoint, ruleEndpoint.Name)
			if err != nil {
				klog.Warningf("built message resource failed with error: %s", err)
				continue
			}
			msg := model.NewMessage("").
				SetResourceVersion(ruleEndpoint.ResourceVersion).
				FillBody(ruleEndpoint)
			switch e.Type {
			case watch.Added:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.InsertOperation)
			case watch.Deleted:
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.DeleteOperation)
			case watch.Modified:
				klog.Warningf("ruleEndpoint event type: %s unsupported", e.Type)
				continue
			default:
				klog.Warningf("ruleEndpoint event type: %s unsupported", e.Type)
				continue
			}

			dc.SendMessage(msg)
		}
	}
}

func (dc *DownstreamController) syncMissions() {
	var operation string
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncMission loop")
			return
		case e := <-dc.missionsManager.Events():
			klog.V(4).Infof("Get mission events: event type: %s.", e.Type)
			mission, ok := e.Object.(*edgeclustersv1.Mission)
			if !ok {
				klog.Warningf("object type: %T unsupported", mission)
				continue
			}
			klog.V(4).Infof("Get mission events: mission object: %+v.", mission)
			switch e.Type {
			case watch.Added:
				operation = model.InsertOperation
			case watch.Modified:
				operation = model.UpdateOperation
			case watch.Deleted:
				operation = model.DeleteOperation
			default:
				// unsupported operation, no need to send to any node
				klog.Warningf("Mission event type: %s unsupported", e.Type)
				continue
			}

			// send to all nodes
			dc.lc.EdgeClusters.Range(func(key interface{}, value interface{}) bool {
				clusterName, ok := key.(string)
				if !ok {
					klog.Warning("Failed to assert key to sting")
					return true
				}
				msg := model.NewMessage("")
				msg.SetResourceVersion(mission.ResourceVersion)
				resource, err := messagelayer.BuildResource(clusterName, "default", model.ResourceTypeMission, mission.Name)
				if err != nil {
					klog.Warningf("Built message resource failed with error: %v", err)
					return true
				}
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, operation)
				msg.Content = mission

				dc.SendMessage(msg)
				return true
			})
		}
	}
}

func (dc *DownstreamController) syncEdgeClusters() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Warning("Stop edgecontroller downstream syncEdgeCluster loop")
			return
		case e := <-dc.edgeClusterManager.Events():
			klog.V(4).Infof("Get edgeCluster events: event type: %s.", e.Type)
			edgeCluster, ok := e.Object.(*edgeclustersv1.EdgeCluster)
			if !ok {
				klog.Warningf("object type: %T unsupported", edgeCluster)
				continue
			}
			klog.V(4).Infof("Get edgeCluster events: edgeCluster object: %+v.", edgeCluster)
			switch e.Type {
			case watch.Added:
				fallthrough
			case watch.Modified:
				missionsInEdge := edgeCluster.State.ReceivedMissions
				missionsInEdgeSet := map[string]bool{}
				for _, m := range missionsInEdge {
					missionsInEdgeSet[m] = true
				}

				missionsInCloudSet := map[string]bool{}
				missionList, err := dc.missionLister.List(labels.Everything())
				if err != nil {
					klog.Warningf("Built message resource failed with error: %s", err)
					break
				}
				for _, m := range missionList {
					missionsInCloudSet[m.Name] = true
				}

				if reflect.DeepEqual(missionsInEdgeSet, missionsInCloudSet) {
					break
				}

				msg := model.NewMessage("")
				resource, err := messagelayer.BuildResource(edgeCluster.Name, "default", model.ResourceTypeMissionList, "")
				msg.BuildRouter(modules.EdgeControllerModuleName, constants.GroupResource, resource, model.UpdateOperation)
				msg.Content = missionList

				dc.SendMessage(msg)

			case watch.Deleted:
				dc.lc.DeleteEdgeCluster(edgeCluster.ObjectMeta.Name)

			default:
				// unsupported operation, no need to send to any node
				klog.Warningf("EdgeCluster event type: %s unsupported", e.Type)
				continue
			}
		}
	}
}

// Start DownstreamController
func (dc *DownstreamController) Start() error {
	klog.Info("start downstream controller")
	// pod
	go dc.syncPod()

	// configmap
	go dc.syncConfigMap()

	// secret
	go dc.syncSecret()

	// nodes
	go dc.syncEdgeNodes()

	// rule
	go dc.syncRule()

	// ruleendpoint
	go dc.syncRuleEndpoint()

	// mission
	go dc.syncMissions()

	// edgecluster
	go dc.syncEdgeClusters()

	return nil
}

// initLocating to know configmap and secret should send to which nodes
func (dc *DownstreamController) initLocating() error {
	set := labels.Set{manager.NodeRoleKey: manager.NodeRoleValue}
	selector := labels.SelectorFromSet(set)
	nodes, err := dc.kubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return err
	}
	var status string
	for _, node := range nodes.Items {
		for _, nsc := range node.Status.Conditions {
			if nsc.Type == "Ready" {
				status = string(nsc.Status)
				break
			}
		}
		dc.lc.UpdateEdgeNode(node.ObjectMeta.Name, status)
	}

	pods, err := dc.kubeClient.CoreV1().Pods(v1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		if dc.lc.IsEdgeNode(p.Spec.NodeName) {
			dc.lc.AddOrUpdatePod(p)
		}
	}

	edgeclusters, err := dc.crdClient.EdgeclustersV1().EdgeClusters().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, ec := range edgeclusters.Items {
		// add logic to get edgecluster status
		dc.lc.UpdateEdgeCluster(ec.ObjectMeta.Name, true)
	}

	return nil
}

// NewDownstreamController create a DownstreamController from config
func NewDownstreamController(k8sInformerFactory k8sinformers.SharedInformerFactory, keInformerFactory informers.KubeEdgeCustomInformer,
	crdInformerFactory crdinformers.SharedInformerFactory) (*DownstreamController, error) {
	lc := &manager.LocationCache{}

	podInformer := k8sInformerFactory.Core().V1().Pods()
	podManager, err := manager.NewPodManager(podInformer.Informer())
	if err != nil {
		klog.Warningf("create pod manager failed with error: %s", err)
		return nil, err
	}

	configMapInformer := k8sInformerFactory.Core().V1().ConfigMaps()
	configMapManager, err := manager.NewConfigMapManager(configMapInformer.Informer())
	if err != nil {
		klog.Warningf("create configmap manager failed with error: %s", err)
		return nil, err
	}

	secretInformer := k8sInformerFactory.Core().V1().Secrets()
	secretManager, err := manager.NewSecretManager(secretInformer.Informer())
	if err != nil {
		klog.Warningf("create secret manager failed with error: %s", err)
		return nil, err
	}
	nodeInformer := keInformerFactory.EdgeNode()
	nodesManager, err := manager.NewNodesManager(nodeInformer)
	if err != nil {
		klog.Warningf("Create nodes manager failed with error: %s", err)
		return nil, err
	}

	svcInformer := k8sInformerFactory.Core().V1().Services()
	serviceManager, err := manager.NewServiceManager(svcInformer.Informer())
	if err != nil {
		klog.Warningf("Create service manager failed with error: %s", err)
		return nil, err
	}

	endpointsInformer := k8sInformerFactory.Core().V1().Endpoints()
	endpointsManager, err := manager.NewEndpointsManager(endpointsInformer.Informer())
	if err != nil {
		klog.Warningf("Create endpoints manager failed with error: %s", err)
		return nil, err
	}

	rulesInformer := crdInformerFactory.Rules().V1().Rules().Informer()
	rulesManager, err := manager.NewRuleManager(rulesInformer)
	if err != nil {
		klog.Warningf("Create rulesManager failed with error: %s", err)
		return nil, err
	}

	ruleEndpointsInformer := crdInformerFactory.Rules().V1().RuleEndpoints().Informer()
	ruleEndpointsManager, err := manager.NewRuleEndpointManager(ruleEndpointsInformer)
	if err != nil {
		klog.Warningf("Create ruleEndpointsManager failed with error: %s", err)
		return nil, err
	}

	missionsInformer := crdInformerFactory.Edgeclusters().V1().Missions()
	missionsManager, err := manager.NewMissionManager(missionsInformer.Informer())
	if err != nil {
		klog.Warningf("Create missionsManager failed with error: %s", err)
		return nil, err
	}

	edgeClustersInformer := crdInformerFactory.Edgeclusters().V1().EdgeClusters()
	edgeClusterManager, err := manager.NewEdgeClusterManager(edgeClustersInformer.Informer())
	if err != nil {
		klog.Warningf("Create edgeClusterManager failed with error: %s", err)
		return nil, err
	}

	dc := &DownstreamController{
		kubeClient:           client.GetKubeClient(),
		crdClient:            client.GetCRDClient(),
		podManager:           podManager,
		configmapManager:     configMapManager,
		secretManager:        secretManager,
		nodeManager:          nodesManager,
		serviceManager:       serviceManager,
		endpointsManager:     endpointsManager,
		messageLayer:         messagelayer.NewContextMessageLayer(),
		lc:                   lc,
		svcLister:            svcInformer.Lister(),
		podLister:            podInformer.Lister(),
		rulesManager:         rulesManager,
		ruleEndpointsManager: ruleEndpointsManager,
		missionsManager:      missionsManager,
		edgeClusterManager:   edgeClusterManager,
		missionLister:        missionsInformer.Lister(),
	}
	if err := dc.initLocating(); err != nil {
		return nil, err
	}

	return dc, nil
}

func (dc *DownstreamController) SendMessage(msg *model.Message) {
	if err := dc.messageLayer.Send(*msg); err != nil {
		klog.Warningf("send message failed with error: %s, operation: %s, resource: %s", err, msg.GetOperation(), msg.GetResource())
	} else {
		klog.V(4).Infof("message sent successfully, operation: %s, resource: %s", msg.GetOperation(), msg.GetResource())
	}
}
