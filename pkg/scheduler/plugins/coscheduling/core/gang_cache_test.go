/*
Copyright 2022 The Koordinator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"

	"github.com/koordinator-sh/koordinator/apis/extension"
	"github.com/koordinator-sh/koordinator/apis/thirdparty/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	fakepgclientset "github.com/koordinator-sh/koordinator/apis/thirdparty/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	pgformers "github.com/koordinator-sh/koordinator/apis/thirdparty/scheduler-plugins/pkg/generated/informers/externalversions"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/apis/config"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/apis/config/v1beta3"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/plugins/coscheduling/util"
)

var fakeTimeNowFn = func() time.Time {
	t := time.Time{}
	_ = t.Add(100 * time.Second)
	return t
}

func getTestDefaultCoschedulingArgs(t *testing.T) *config.CoschedulingArgs {
	var v1beta3args v1beta3.CoschedulingArgs
	v1beta3.SetDefaults_CoschedulingArgs(&v1beta3args)
	var args config.CoschedulingArgs
	err := v1beta3.Convert_v1beta3_CoschedulingArgs_To_config_CoschedulingArgs(&v1beta3args, &args, nil)
	assert.NoError(t, err)
	return &args
}

func TestGangCache_OnPodAdd(t *testing.T) {
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	defaultArgs := getTestDefaultCoschedulingArgs(t)
	tests := []struct {
		name          string
		pods          []*corev1.Pod
		wantCache     map[string]*Gang
		onceSatisfied bool
	}{
		{
			name:      "add invalid pod",
			pods:      []*corev1.Pod{{}},
			wantCache: map[string]*Gang{},
		},
		{
			name: "add invalid pod2",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"test": "gang"},
						Annotations: map[string]string{"test": "gang"},
					},
				},
			},
			wantCache: map[string]*Gang{},
		},
		{
			name: "add pod announcing Gang in CRD way before CRD created,gang should be created but not initialized",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "crdPod",
						Namespace: "default",
						Labels:    map[string]string{v1alpha1.PodGroupLabel: "test"},
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/test": {
					Name:            "default/test",
					CreateTime:      fakeTimeNowFn(),
					WaitTime:        0,
					GangGroupId:     "default/test",
					GangGroup:       []string{"default/test"},
					GangGroupInfo:   NewGangGroupInfo("", nil),
					Mode:            extension.GangModeStrict,
					GangFrom:        GangFromPodAnnotation,
					GangMatchPolicy: extension.GangMatchPolicyOnceSatisfied,
					HasGangInit:     false,
					Children: map[string]*corev1.Pod{
						"default/crdPod": {
							ObjectMeta: metav1.ObjectMeta{
								Name:      "crdPod",
								Namespace: "default",
								Labels:    map[string]string{v1alpha1.PodGroupLabel: "test"},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/crdPod": {
							ObjectMeta: metav1.ObjectMeta{
								Name:      "crdPod",
								Namespace: "default",
								Labels:    map[string]string{v1alpha1.PodGroupLabel: "test"},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
			},
		},
		{
			name: "add pod announcing Gang in Annotation way",
			pods: []*corev1.Pod{
				// pod1 announce GangA
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod1",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "ganga",
							extension.AnnotationGangMinNum:   "2",
							extension.AnnotationGangWaitTime: "30s",
							extension.AnnotationGangMode:     extension.GangModeNonStrict,
							extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
						},
					},
					Spec: corev1.PodSpec{
						NodeName: "nba",
					},
				},
				// pod2 also announce GangA but with different annotations after pod1's announcing
				// so gangA in cache should only be created with pod1's Annotations
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod2",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "ganga",
							extension.AnnotationGangMinNum:   "7",
							extension.AnnotationGangWaitTime: "3000s",
							extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
						},
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/ganga": {
					Name:              "default/ganga",
					WaitTime:          30 * time.Second,
					CreateTime:        fakeTimeNowFn(),
					Mode:              extension.GangModeNonStrict,
					MinRequiredNumber: 2,
					TotalChildrenNum:  2,
					GangGroup:         []string{"default/ganga", "default/gangb"},
					GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/ganga", "default/gangb"}), []string{"default/ganga", "default/gangb"}),
					HasGangInit:       true,
					GangFrom:          GangFromPodAnnotation,
					GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
					Children: map[string]*corev1.Pod{
						"default/pod1": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod1",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "2",
									extension.AnnotationGangWaitTime: "30s",
									extension.AnnotationGangMode:     extension.GangModeNonStrict,
									extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
								},
							},
							Spec: corev1.PodSpec{
								NodeName: "nba",
							},
						},
						"default/pod2": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod2",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "7",
									extension.AnnotationGangWaitTime: "3000s",
									extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
								},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/pod2": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod2",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "7",
									extension.AnnotationGangWaitTime: "3000s",
									extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
								},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren: map[string]*corev1.Pod{
						"default/pod1": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod1",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "2",
									extension.AnnotationGangWaitTime: "30s",
									extension.AnnotationGangMode:     extension.GangModeNonStrict,
									extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
								},
							},
							Spec: corev1.PodSpec{
								NodeName: "nba",
							},
						},
					},
				},
			},
			onceSatisfied: true,
		},
		{
			name: "add pod announcing Gang in lightweight-coscheduling way",
			pods: []*corev1.Pod{
				// pod1 announce GangA
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod1",
						Labels: map[string]string{
							// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupName is deprecated
							extension.LabelLightweightCoschedulingPodGroupName: "ganga",
							// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupMinAvailable is deprecated
							extension.LabelLightweightCoschedulingPodGroupMinAvailable: "2",
						},
					},
					Spec: corev1.PodSpec{
						NodeName: "nba",
					},
				},
				// pod2 also announce GangA but with different annotations after pod1's announcing
				// so gangA in cache should only be created with pod1's Annotations
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod2",
						Labels: map[string]string{
							// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupName is deprecated
							extension.LabelLightweightCoschedulingPodGroupName: "ganga",
							// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupMinAvailable is deprecated
							extension.LabelLightweightCoschedulingPodGroupMinAvailable: "2",
						},
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/ganga": {
					Name:              "default/ganga",
					WaitTime:          defaultArgs.DefaultTimeout.Duration,
					CreateTime:        fakeTimeNowFn(),
					Mode:              extension.GangModeStrict,
					MinRequiredNumber: 2,
					TotalChildrenNum:  2,
					GangGroup:         []string{"default/ganga"},
					GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/ganga"}), []string{"default/ganga"}),
					HasGangInit:       true,
					GangFrom:          GangFromPodAnnotation,
					GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
					Children: map[string]*corev1.Pod{
						"default/pod1": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod1",
								Labels: map[string]string{
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupName is deprecated
									extension.LabelLightweightCoschedulingPodGroupName: "ganga",
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupMinAvailable is deprecated
									extension.LabelLightweightCoschedulingPodGroupMinAvailable: "2",
								},
							},
							Spec: corev1.PodSpec{
								NodeName: "nba",
							},
						},
						"default/pod2": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod2",
								Labels: map[string]string{
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupName is deprecated
									extension.LabelLightweightCoschedulingPodGroupName: "ganga",
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupMinAvailable is deprecated
									extension.LabelLightweightCoschedulingPodGroupMinAvailable: "2",
								},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/pod2": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod2",
								Labels: map[string]string{
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupName is deprecated
									extension.LabelLightweightCoschedulingPodGroupName: "ganga",
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupMinAvailable is deprecated
									extension.LabelLightweightCoschedulingPodGroupMinAvailable: "2",
								},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren: map[string]*corev1.Pod{
						"default/pod1": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod1",
								Labels: map[string]string{
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupName is deprecated
									extension.LabelLightweightCoschedulingPodGroupName: "ganga",
									// nolint:staticcheck // SA1019: extension.LabelLightweightCoschedulingPodGroupMinAvailable
									extension.LabelLightweightCoschedulingPodGroupMinAvailable: "2",
								},
							},
							Spec: corev1.PodSpec{
								NodeName: "nba",
							},
						},
					},
				},
			},
			onceSatisfied: true,
		},
		{
			name: "add pods announcing Gang in Annotation way,but with illegal args",
			pods: []*corev1.Pod{
				// pod3 announce GangB with illegal minNum,
				// so that gangA's info depends on the next pod's Annotations
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod3",
						Annotations: map[string]string{
							extension.AnnotationGangName:   "gangb",
							extension.AnnotationGangMinNum: "xxx",
						},
					},
				},
				// pod4 also announce GangA but with legal minNum,illegal remaining args
				// so gangA in cache should only be created with pod4's Annotations(illegal args set by default)
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod4",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "gangb",
							extension.AnnotationGangMinNum:   "2",
							extension.AnnotationGangTotalNum: "1",
							extension.AnnotationGangMode:     "WenShiqi222",
							extension.AnnotationGangWaitTime: "WenShiqi222",
							extension.AnnotationGangGroups:   "ganga,gangx",
						},
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/gangb": {
					Name:              "default/gangb",
					WaitTime:          defaultArgs.DefaultTimeout.Duration,
					CreateTime:        fakeTimeNowFn(),
					Mode:              extension.GangModeStrict,
					MinRequiredNumber: 2,
					TotalChildrenNum:  2,
					GangGroup:         []string{"default/gangb"},
					GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangb"}), []string{"default/gangb"}),
					GangGroupId:       "default/gangb",
					HasGangInit:       true,
					GangFrom:          GangFromPodAnnotation,
					GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
					Children: map[string]*corev1.Pod{
						"default/pod3": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod3",
								Annotations: map[string]string{
									extension.AnnotationGangName:   "gangb",
									extension.AnnotationGangMinNum: "xxx",
								},
							},
						},
						"default/pod4": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod4",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "gangb",
									extension.AnnotationGangMinNum:   "2",
									extension.AnnotationGangTotalNum: "1",
									extension.AnnotationGangMode:     "WenShiqi222",
									extension.AnnotationGangWaitTime: "WenShiqi222",
									extension.AnnotationGangGroups:   "ganga,gangx",
								},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/pod3": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod3",
								Annotations: map[string]string{
									extension.AnnotationGangName:   "gangb",
									extension.AnnotationGangMinNum: "xxx",
								},
							},
						},
						"default/pod4": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod4",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "gangb",
									extension.AnnotationGangMinNum:   "2",
									extension.AnnotationGangTotalNum: "1",
									extension.AnnotationGangMode:     "WenShiqi222",
									extension.AnnotationGangWaitTime: "WenShiqi222",
									extension.AnnotationGangGroups:   "ganga,gangx",
								},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
			},
		},
		{
			name: "add pods announcing Gang in Annotation way,but with illegal args",
			pods: []*corev1.Pod{
				// pod1 announce GangA with illegal AnnotationGangWaitTime,
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod5",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "gangc",
							extension.AnnotationGangMinNum:   "0",
							extension.AnnotationGangWaitTime: "0",
							extension.AnnotationGangGroups:   "[a,b]",
						},
					},
				},
				// pod2 announce GangB with illegal AnnotationGangWaitTime,
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod6",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "gangd",
							extension.AnnotationGangMinNum:   "0",
							extension.AnnotationGangWaitTime: "-20s",
							extension.AnnotationGangGroups:   "[a,b]",
						},
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/gangc": {
					Name:              "default/gangc",
					WaitTime:          defaultArgs.DefaultTimeout.Duration,
					CreateTime:        fakeTimeNowFn(),
					Mode:              extension.GangModeStrict,
					GangGroupId:       "default/gangc",
					MinRequiredNumber: 0,
					TotalChildrenNum:  0,
					GangGroup:         []string{"default/gangc"},
					GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangc"}), []string{"default/gangc"}),
					HasGangInit:       true,
					GangFrom:          GangFromPodAnnotation,
					GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
					Children: map[string]*corev1.Pod{
						"default/pod5": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod5",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "gangc",
									extension.AnnotationGangMinNum:   "0",
									extension.AnnotationGangWaitTime: "0",
									extension.AnnotationGangGroups:   "[a,b]",
								},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/pod5": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod5",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "gangc",
									extension.AnnotationGangMinNum:   "0",
									extension.AnnotationGangWaitTime: "0",
									extension.AnnotationGangGroups:   "[a,b]",
								},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
				"default/gangd": {
					Name:              "default/gangd",
					WaitTime:          defaultArgs.DefaultTimeout.Duration,
					CreateTime:        fakeTimeNowFn(),
					Mode:              extension.GangModeStrict,
					GangGroupId:       "default/gangd",
					GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangd"}), []string{"default/gangd"}),
					MinRequiredNumber: 0,
					TotalChildrenNum:  0,
					GangGroup:         []string{"default/gangd"},
					HasGangInit:       true,
					GangFrom:          GangFromPodAnnotation,
					GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
					Children: map[string]*corev1.Pod{
						"default/pod6": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod6",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "gangd",
									extension.AnnotationGangMinNum:   "0",
									extension.AnnotationGangWaitTime: "-20s",
									extension.AnnotationGangGroups:   "[a,b]",
								},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/pod6": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod6",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "gangd",
									extension.AnnotationGangMinNum:   "0",
									extension.AnnotationGangWaitTime: "-20s",
									extension.AnnotationGangGroups:   "[a,b]",
								},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgClientSet := fakepgclientset.NewSimpleClientset()
			pgInformerFactory := pgformers.NewSharedInformerFactory(pgClientSet, 0)
			pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
			pglister := pgInformer.Lister()

			gangCache := NewGangCache(defaultArgs, nil, pglister, pgClientSet, nil)
			for _, pod := range tt.pods {
				gangCache.onPodAdd(pod)
			}
			for k, v := range tt.wantCache {
				if !v.HasGangInit {
					continue
				}
				tt.wantCache[k].GangGroupId = util.GetGangGroupId(v.GangGroup)
				tt.wantCache[k].GangGroupInfo.OnceResourceSatisfied = tt.onceSatisfied
			}

			for _, pod := range tt.pods {
				gangName := util.GetGangNameByPod(pod)
				gangId := util.GetId(pod.Namespace, gangName)
				gang := tt.wantCache[gangId]
				if gang == nil {
					continue
				}

				if gang.GangGroupInfo == nil {
					continue
				}

				if gangCache.gangItems[gangId].GangGroupInfo.IsInitialized() {
					gang.GangGroupInfo.SetInitialized()

				}
			}

			assert.Equal(t, tt.wantCache, gangCache.gangItems)
		})
	}
}

func TestGangCache_OnPodUpdate(t *testing.T) {
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	defaultArgs := getTestDefaultCoschedulingArgs(t)
	tests := []struct {
		name          string
		pods          []*corev1.Pod
		wantCache     map[string]*Gang
		onceSatisfied bool
	}{
		{
			name:      "add invalid pod",
			pods:      []*corev1.Pod{{}},
			wantCache: map[string]*Gang{},
		},
		{
			name: "add pod announcing Gang in Annotation way",
			pods: []*corev1.Pod{
				// pod1 announce GangA
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod1",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "ganga",
							extension.AnnotationGangMinNum:   "2",
							extension.AnnotationGangWaitTime: "30s",
							extension.AnnotationGangMode:     extension.GangModeNonStrict,
							extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
						},
					},
					Spec: corev1.PodSpec{
						NodeName: "nba",
					},
				},
				// pod2 also announce GangA but with different annotations after pod1's announcing
				// so gangA in cache should only be created with pod1's Annotations
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod2",
						Annotations: map[string]string{
							extension.AnnotationGangName:     "ganga",
							extension.AnnotationGangMinNum:   "7",
							extension.AnnotationGangWaitTime: "3000s",
							extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
						},
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/ganga": {
					Name:              "default/ganga",
					WaitTime:          30 * time.Second,
					CreateTime:        fakeTimeNowFn(),
					Mode:              extension.GangModeNonStrict,
					GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
					MinRequiredNumber: 2,
					TotalChildrenNum:  2,
					GangGroup:         []string{"default/ganga", "default/gangb"},
					GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/ganga", "default/gangb"}), []string{"default/ganga", "default/gangb"}),
					HasGangInit:       true,
					GangFrom:          GangFromPodAnnotation,
					Children: map[string]*corev1.Pod{
						"default/pod1": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod1",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "2",
									extension.AnnotationGangWaitTime: "30s",
									extension.AnnotationGangMode:     extension.GangModeNonStrict,
									extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
								},
							},
							Spec: corev1.PodSpec{
								NodeName: "nba",
							},
						},
						"default/pod2": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod2",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "7",
									extension.AnnotationGangWaitTime: "3000s",
									extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
								},
							},
						},
					},
					PendingChildren: map[string]*corev1.Pod{
						"default/pod2": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod2",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "7",
									extension.AnnotationGangWaitTime: "3000s",
									extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
								},
							},
						},
					},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren: map[string]*corev1.Pod{
						"default/pod1": {
							ObjectMeta: metav1.ObjectMeta{
								Namespace: "default",
								Name:      "pod1",
								Annotations: map[string]string{
									extension.AnnotationGangName:     "ganga",
									extension.AnnotationGangMinNum:   "2",
									extension.AnnotationGangWaitTime: "30s",
									extension.AnnotationGangMode:     extension.GangModeNonStrict,
									extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
								},
							},
							Spec: corev1.PodSpec{
								NodeName: "nba",
							},
						},
					},
				},
			},
			onceSatisfied: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgClientSet := fakepgclientset.NewSimpleClientset()
			pgInformerFactory := pgformers.NewSharedInformerFactory(pgClientSet, 0)
			pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
			pglister := pgInformer.Lister()

			gangCache := NewGangCache(defaultArgs, nil, pglister, pgClientSet, nil)
			for _, pod := range tt.pods {
				gangCache.onPodUpdate(pod, pod)
			}
			for k, v := range tt.wantCache {
				if !v.HasGangInit {
					continue
				}
				tt.wantCache[k].GangGroupId = util.GetGangGroupId(v.GangGroup)
				tt.wantCache[k].GangGroupInfo.OnceResourceSatisfied = true
			}

			for _, pod := range tt.pods {
				gangName := util.GetGangNameByPod(pod)
				gangId := util.GetId(pod.Namespace, gangName)
				gang := tt.wantCache[gangId]
				if gang == nil {
					continue
				}

				if gangCache.gangItems[gangId].GangGroupInfo.IsInitialized() {
					gang.GangGroupInfo.SetInitialized()

				}
			}

			assert.Equal(t, tt.wantCache, gangCache.gangItems)
		})
	}
}

func TestGangCache_OnPodDelete(t *testing.T) {
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	tests := []struct {
		name         string
		podGroups    []*v1alpha1.PodGroup
		pods         []*corev1.Pod
		wantCache    map[string]*Gang
		wantPodGroup map[string]*v1alpha1.PodGroup
	}{
		{
			name: "delete invalid pod,has no gang",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod1",
					},
				},
			},
			wantCache: map[string]*Gang{},
		},
		{
			name: "delete invalid pod2,gang has not find",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pod2",
						Namespace:   "wenshiqi",
						Labels:      map[string]string{"test": "gang"},
						Annotations: map[string]string{"test": "gang"},
					},
				},
			},
			wantCache: map[string]*Gang{},
		},
		{
			name: "delete gangA's pods one by one,finally gangA should be deleted",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod3",
						Annotations: map[string]string{
							extension.AnnotationGangName: "gangA",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod4",
						Annotations: map[string]string{
							extension.AnnotationGangName: "gangA",
						},
					},
				},
			},
			wantCache: map[string]*Gang{},
			wantPodGroup: map[string]*v1alpha1.PodGroup{
				"gangA": nil,
			},
		},
		{
			name: "delete gangB's pods one by one,but gangB is created by CRD",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod5",
						Labels: map[string]string{
							v1alpha1.PodGroupLabel: "gangB",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "pod6",
						Labels: map[string]string{
							v1alpha1.PodGroupLabel: "gangB",
						},
					},
				},
			},
			podGroups: []*v1alpha1.PodGroup{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "gangB",
					},
					Spec: v1alpha1.PodGroupSpec{
						MinMember:              4,
						ScheduleTimeoutSeconds: pointer.Int32(10),
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/gangB": {
					Name:                   "default/gangB",
					WaitTime:               10 * time.Second,
					CreateTime:             fakeTimeNowFn(),
					Mode:                   extension.GangModeStrict,
					MinRequiredNumber:      4,
					TotalChildrenNum:       4,
					GangGroup:              []string{"default/gangB"},
					GangGroupId:            "default/gangB",
					GangGroupInfo:          NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangB"}), []string{"default/gangB"}),
					HasGangInit:            true,
					GangFrom:               GangFromPodGroupCrd,
					GangMatchPolicy:        extension.GangMatchPolicyOnceSatisfied,
					Children:               map[string]*corev1.Pod{},
					PendingChildren:        map[string]*corev1.Pod{},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
			},
			wantPodGroup: map[string]*v1alpha1.PodGroup{
				"gangB": {
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "gangB",
					},
					Spec: v1alpha1.PodGroupSpec{
						MinMember:              4,
						ScheduleTimeoutSeconds: pointer.Int32(10),
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgClient := fakepgclientset.NewSimpleClientset()
			pgInformerFactory := pgformers.NewSharedInformerFactory(pgClient, 0)
			pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
			pglister := pgInformer.Lister()
			gangCache := NewGangCache(&config.CoschedulingArgs{DefaultTimeout: metav1.Duration{Duration: time.Second}}, nil, pglister, pgClient, nil)
			for _, pg := range tt.podGroups {
				err := retry.OnError(
					retry.DefaultRetry,
					errors.IsTooManyRequests,
					func() error {
						var err error
						pg, err = pgClient.SchedulingV1alpha1().PodGroups("default").Create(context.TODO(), pg, metav1.CreateOptions{})
						return err
					})
				if err != nil {
					t.Errorf("retry pgClient create PodGroup err: %v", err)
				}
				gangCache.onPodGroupAdd(pg)
			}
			for _, pod := range tt.pods {
				gangCache.onPodAdd(pod)
			}

			for _, pod := range tt.pods {
				gangName := util.GetGangNameByPod(pod)
				gangId := util.GetId(pod.Namespace, gangName)
				gang := tt.wantCache[gangId]
				if gang == nil {
					continue
				}

				if gangCache.gangItems[gangId].GangGroupInfo.IsInitialized() {
					gang.GangGroupInfo.SetInitialized()
				}
			}

			// start deleting pods
			for _, pod := range tt.pods {
				gangCache.onPodDelete(pod)
			}
			for k, v := range tt.wantCache {
				if !v.HasGangInit {
					continue
				}
				tt.wantCache[k].GangGroupId = util.GetGangGroupId(v.GangGroup)
			}
			assert.Equal(t, tt.wantCache, gangCache.gangItems)

			for pgKey, pgT := range tt.wantPodGroup {
				var pg *v1alpha1.PodGroup
				err := retry.OnError(
					retry.DefaultRetry,
					errors.IsTooManyRequests,
					func() error {
						var err error
						pg, err = pgClient.SchedulingV1alpha1().PodGroups("default").Get(context.TODO(), pgKey, metav1.GetOptions{})
						return err
					})
				// pgT ==nil, we can not get the pg from the cluster,error should be nil
				if pgT == nil {
					if err == nil {
						t.Error()
					}
				} else {
					if err != nil {
						t.Errorf("retry pgClient Get PodGroup err: %v", err)
					} else {
						assert.Equal(t, pgT, pg)
					}
				}
			}
		})
	}
}

func TestGangCache_OnPodGroupAdd(t *testing.T) {
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	waitTime := int32(300)
	tests := []struct {
		name      string
		pgs       []*v1alpha1.PodGroup
		wantCache map[string]*Gang
	}{
		{
			name: "update podGroup with annotations",
			pgs: []*v1alpha1.PodGroup{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "gangA",
						Annotations: map[string]string{
							extension.AnnotationGangMode:   extension.GangModeNonStrict,
							extension.AnnotationGangGroups: "[\"default/gangA\",\"default/gangB\"]",
						},
					},
					Spec: v1alpha1.PodGroupSpec{
						MinMember:              2,
						ScheduleTimeoutSeconds: &waitTime,
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/gangA": {
					Name:                   "default/gangA",
					WaitTime:               300 * time.Second,
					CreateTime:             fakeTimeNowFn(),
					Mode:                   extension.GangModeNonStrict,
					MinRequiredNumber:      2,
					TotalChildrenNum:       2,
					GangGroup:              []string{"default/gangA", "default/gangB"},
					GangGroupInfo:          NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangA", "default/gangB"}), []string{"default/gangA", "default/gangB"}),
					HasGangInit:            true,
					GangFrom:               GangFromPodGroupCrd,
					GangMatchPolicy:        extension.GangMatchPolicyOnceSatisfied,
					PendingChildren:        map[string]*corev1.Pod{},
					Children:               map[string]*corev1.Pod{},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
			},
		},
		{
			name: "update podGroup with illegal annotations",
			pgs: []*v1alpha1.PodGroup{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "gangA",
						Annotations: map[string]string{
							extension.AnnotationGangMode:     "WenShiqi222",
							extension.AnnotationGangGroups:   "a,b",
							extension.AnnotationGangTotalNum: "2",
						},
					},
					Spec: v1alpha1.PodGroupSpec{
						MinMember:              4,
						ScheduleTimeoutSeconds: &waitTime,
					},
				},
			},
			wantCache: map[string]*Gang{
				"default/gangA": {
					Name:                   "default/gangA",
					WaitTime:               300 * time.Second,
					CreateTime:             fakeTimeNowFn(),
					Mode:                   extension.GangModeStrict,
					MinRequiredNumber:      4,
					TotalChildrenNum:       4,
					GangGroup:              []string{"default/gangA"},
					GangGroupId:            "default/gangA",
					GangGroupInfo:          NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangA"}), []string{"default/gangA"}),
					HasGangInit:            true,
					GangFrom:               GangFromPodGroupCrd,
					GangMatchPolicy:        extension.GangMatchPolicyOnceSatisfied,
					PendingChildren:        map[string]*corev1.Pod{},
					Children:               map[string]*corev1.Pod{},
					WaitingForBindChildren: map[string]*corev1.Pod{},
					BoundChildren:          map[string]*corev1.Pod{},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgClient := fakepgclientset.NewSimpleClientset()
			gangCache := NewGangCache(&config.CoschedulingArgs{DefaultTimeout: metav1.Duration{Duration: time.Second}}, nil, nil, pgClient, nil)
			for _, pg := range tt.pgs {
				gangCache.onPodGroupAdd(pg)
			}

			for _, gang := range tt.wantCache {

				if gangCache.gangItems[gang.Name].GangGroupInfo.IsInitialized() {
					gang.GangGroupInfo.SetInitialized()
				}
			}

			for k, v := range tt.wantCache {
				if !v.HasGangInit {
					continue
				}
				tt.wantCache[k].GangGroupId = util.GetGangGroupId(v.GangGroup)
			}
			assert.Equal(t, tt.wantCache, gangCache.gangItems)
		})
	}
}

func TestGangCache_OnGangDelete(t *testing.T) {
	pgClient := fakepgclientset.NewSimpleClientset()
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pglister := pgInformer.Lister()
	cache := NewGangCache(&config.CoschedulingArgs{}, nil, pglister, pgClient, nil)

	// case1: pg that created by crd,delete pg then will delete the gang
	podGroup := &v1alpha1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "ganga",
		},
	}
	gangId := util.GetId("default", "ganga")
	gangTmp := cache.getGangFromCacheByGangId(gangId, true)
	gangTmp.GangGroupInfo = NewGangGroupInfo("", nil)

	cache.onPodGroupDelete(podGroup)
	assert.Equal(t, 0, len(cache.gangItems))

	// case2: pg that created by annotations,pg deleted will do nothing

	podToCreatePg := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod1",
			Annotations: map[string]string{
				extension.AnnotationGangName:     "gangb",
				extension.AnnotationGangMinNum:   "2",
				extension.AnnotationGangWaitTime: "30s",
				extension.AnnotationGangMode:     extension.GangModeNonStrict,
				extension.AnnotationGangGroups:   "[\"default/gangA\",\"default/gangB\"]",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "nba",
		},
	}

	cache.onPodAdd(podToCreatePg)

	wantedGang := &Gang{
		Name:              "default/gangb",
		WaitTime:          30 * time.Second,
		CreateTime:        fakeTimeNowFn(),
		Mode:              extension.GangModeNonStrict,
		MinRequiredNumber: 2,
		TotalChildrenNum:  2,
		GangGroup:         []string{"default/gangA", "default/gangB"},
		GangGroupInfo:     NewGangGroupInfo(util.GetGangGroupId([]string{"default/gangA", "default/gangB"}), []string{"default/gangA", "default/gangB"}),
		HasGangInit:       true,
		GangFrom:          GangFromPodAnnotation,
		GangMatchPolicy:   extension.GangMatchPolicyOnceSatisfied,
		Children: map[string]*corev1.Pod{
			"default/pod1": {
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "pod1",
					Annotations: map[string]string{
						extension.AnnotationGangName:     "gangb",
						extension.AnnotationGangMinNum:   "2",
						extension.AnnotationGangWaitTime: "30s",
						extension.AnnotationGangMode:     extension.GangModeNonStrict,
						extension.AnnotationGangGroups:   "[\"default/gangA\",\"default/gangB\"]",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: "nba",
				},
			},
		},
		PendingChildren:        map[string]*corev1.Pod{},
		WaitingForBindChildren: map[string]*corev1.Pod{},
		BoundChildren: map[string]*corev1.Pod{
			"default/pod1": {
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "pod1",
					Annotations: map[string]string{
						extension.AnnotationGangName:     "gangb",
						extension.AnnotationGangMinNum:   "2",
						extension.AnnotationGangWaitTime: "30s",
						extension.AnnotationGangMode:     extension.GangModeNonStrict,
						extension.AnnotationGangGroups:   "[\"default/gangA\",\"default/gangB\"]",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: "nba",
				},
			},
		},
	}
	wantedGang.GangGroupInfo.OnceResourceSatisfied = true

	cacheGang := cache.getGangFromCacheByGangId("default/gangb", false)
	wantedGang.GangGroupId = util.GetGangGroupId(wantedGang.GangGroup)

	if cacheGang.GangGroupInfo.IsInitialized() {
		wantedGang.GangGroupInfo.SetInitialized()
	}

	assert.Equal(t, wantedGang, cacheGang)
}

func TestGangCache_onPodGroupUpdate(t *testing.T) {
	pgClient := fakepgclientset.NewSimpleClientset()
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pglister := pgInformer.Lister()
	cache := NewGangCache(&config.CoschedulingArgs{DefaultTimeout: metav1.Duration{Duration: time.Second}}, nil, pglister, pgClient, nil)

	// init gang
	podGroup := &v1alpha1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "ganga",
		},
		Spec: v1alpha1.PodGroupSpec{
			MinMember: 2,
		},
	}
	gangId := util.GetId("default", "ganga")
	cache.onPodGroupAdd(podGroup)
	gang := cache.getGangFromCacheByGangId(gangId, false)
	assert.Equal(t, gang.MinRequiredNumber, int(podGroup.Spec.MinMember))

	// update gang
	newPodGroup := podGroup.DeepCopy()
	newPodGroup.Spec.MinMember = 3
	cache.onPodGroupUpdate(podGroup, newPodGroup)
	gang = cache.getGangFromCacheByGangId(gangId, false)
	assert.Equal(t, gang.MinRequiredNumber, int(newPodGroup.Spec.MinMember))
}

func TestGetGangGroupInfo_DeleteGangGroupInfo(t *testing.T) {
	pgClient := fakepgclientset.NewSimpleClientset()
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pglister := pgInformer.Lister()
	cache := NewGangCache(&config.CoschedulingArgs{DefaultTimeout: metav1.Duration{Duration: time.Second}}, nil, pglister, pgClient, nil)

	gangGroupInfo := cache.getGangGroupInfo("aa", []string{"aa"}, false)
	assert.True(t, gangGroupInfo == nil)
	assert.Equal(t, 0, len(cache.gangGroupInfoMap))

	gangGroupInfo = cache.getGangGroupInfo("aa", []string{"aa"}, true)
	assert.True(t, gangGroupInfo != nil)
	assert.Equal(t, gangGroupInfo.GangGroupId, "aa")
	assert.Equal(t, gangGroupInfo.GangGroup, []string{"aa"})
	assert.Equal(t, 1, len(cache.gangGroupInfoMap))

	gangGroupInfo = cache.getGangGroupInfo("aa", []string{"aa"}, false)
	assert.True(t, gangGroupInfo != nil)
	assert.Equal(t, gangGroupInfo.GangGroupId, "aa")
	assert.Equal(t, gangGroupInfo.GangGroup, []string{"aa"})
	assert.Equal(t, 1, len(cache.gangGroupInfoMap))

	cache.deleteGangGroupInfo("aa")
	gangGroupInfo = cache.getGangGroupInfo("aa", []string{"aa"}, false)
	assert.True(t, gangGroupInfo == nil)
	assert.Equal(t, 0, len(cache.gangGroupInfoMap))
}

func TestOnPodAdd_OnPodDeleteWithGangGroupInfo(t *testing.T) {
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	defaultArgs := getTestDefaultCoschedulingArgs(t)

	pods := []*corev1.Pod{
		// pod1 announce GangA
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "pod1",
				Annotations: map[string]string{
					extension.AnnotationGangName:     "ganga",
					extension.AnnotationGangMinNum:   "2",
					extension.AnnotationGangWaitTime: "30s",
					extension.AnnotationGangMode:     extension.GangModeNonStrict,
					extension.AnnotationGangGroups:   "[\"default/ganga\",\"default/gangb\"]",
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "nba",
			},
		},
		// pod2 also announce GangA but with different annotations after pod1's announcing
		// so gangA in cache should only be created with pod1's Annotations
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "pod2",
				Annotations: map[string]string{
					extension.AnnotationGangName:     "ganga",
					extension.AnnotationGangMinNum:   "7",
					extension.AnnotationGangWaitTime: "3000s",
					extension.AnnotationGangGroups:   "[\"default/gangc\",\"default/gangd\"]",
				},
			},
		},
	}

	pgClientSet := fakepgclientset.NewSimpleClientset()
	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClientSet, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pglister := pgInformer.Lister()

	gangCache := NewGangCache(defaultArgs, nil, pglister, pgClientSet, nil)
	for _, pod := range pods {
		gangCache.onPodAdd(pod)
	}

	gangName := util.GetGangNameByPod(pods[0])
	gangNamespace := pods[0].Namespace
	gangId := util.GetId(gangNamespace, gangName)
	gang := gangCache.getGangFromCacheByGangId(gangId, false)

	assert.Equal(t, 1, len(gangCache.gangGroupInfoMap))
	assert.Equal(t, util.GetGangGroupId(gang.GangGroup), gang.GangGroupInfo.GangGroupId)

	gangCache.onPodDelete(pods[0])
	assert.Equal(t, 1, len(gangCache.gangGroupInfoMap))
	assert.Equal(t, util.GetGangGroupId(gang.GangGroup), gang.GangGroupInfo.GangGroupId)

	gangCache.onPodDelete(pods[1])
	assert.Equal(t, 0, len(gangCache.gangGroupInfoMap))
}

func TestOnPgAdd_OnPgDeleteWithGangGroupInfo(t *testing.T) {
	preTimeNowFn := timeNowFn
	defer func() {
		timeNowFn = preTimeNowFn
	}()
	timeNowFn = fakeTimeNowFn

	defaultArgs := getTestDefaultCoschedulingArgs(t)

	pgs := []*v1alpha1.PodGroup{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "ganga",
				Annotations: map[string]string{
					extension.AnnotationGangMode: extension.GangModeNonStrict,
				},
			},
			Spec: v1alpha1.PodGroupSpec{
				MinMember: 2,
			},
		},
	}

	pods := []*corev1.Pod{
		// pod1 announce GangA
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   "default",
				Name:        "pod1",
				Annotations: map[string]string{},
				Labels: map[string]string{
					v1alpha1.PodGroupLabel: "ganga",
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "nba",
			},
		},
		// pod2 also announce GangA but with different annotations after pod1's announcing
		// so gangA in cache should only be created with pod1's Annotations
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   "default",
				Name:        "pod2",
				Annotations: map[string]string{},
				Labels: map[string]string{
					v1alpha1.PodGroupLabel: "ganga",
				},
			},
		},
	}

	pgClientSet := fakepgclientset.NewSimpleClientset()
	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClientSet, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pglister := pgInformer.Lister()

	gangCache := NewGangCache(defaultArgs, nil, pglister, pgClientSet, nil)

	gangCache.onPodAdd(pods[0])

	gangName := util.GetGangNameByPod(pods[0])
	gangNamespace := pods[0].Namespace
	gangId := util.GetId(gangNamespace, gangName)
	gang := gangCache.getGangFromCacheByGangId(gangId, false)

	assert.Equal(t, 0, len(gangCache.gangGroupInfoMap))

	gangCache.onPodGroupAdd(pgs[0])
	assert.Equal(t, 1, len(gangCache.gangGroupInfoMap))
	assert.Equal(t, util.GetGangGroupId(gang.GangGroup), gang.GangGroupInfo.GangGroupId)

	gangCache.onPodAdd(pods[1])
	assert.Equal(t, 1, len(gangCache.gangGroupInfoMap))
	assert.Equal(t, util.GetGangGroupId(gang.GangGroup), gang.GangGroupInfo.GangGroupId)

	gangCache.onPodDelete(pods[0])
	assert.Equal(t, 1, len(gangCache.gangGroupInfoMap))
	assert.Equal(t, util.GetGangGroupId(gang.GangGroup), gang.GangGroupInfo.GangGroupId)

	gangCache.onPodDelete(pods[1])
	assert.Equal(t, 1, len(gangCache.gangGroupInfoMap))
	assert.Equal(t, util.GetGangGroupId(gang.GangGroup), gang.GangGroupInfo.GangGroupId)

	gangCache.onPodGroupDelete(pgs[0])
	assert.Equal(t, 0, len(gangCache.gangGroupInfoMap))
}
