package filestorecsi

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	betapb "github.com/cloudburrow/cloudburrow/internal/service/filestore/filestorev1beta1pb"
)

// Multishare volumes (#2194): the `enterprise-multishare-rwx` class, as
// https://docs.cloud.google.com/filestore/docs/multishares documents GKE's
// driver (read 2026-11-23). Each claim is a share (10 GiB to
// max-volume-size, 1 TiB by default) of an Enterprise multishare instance
// of the class; the driver packs shares onto the class's instances, grows
// an instance in 256 GiB steps to take a new or larger share, makes a new
// instance once an instance has max_share_count (10 TiB / max-volume-size)
// shares or no room to grow, shrinks an instance when a share goes, and
// deletes it with its last share. Volume handles are
// modeMultishare/CLASS_LABEL/PROJECT/LOCATION/INSTANCE/SHARE, as GKE's are.

// MultishareClass is GKE's multishare StorageClass.
const MultishareClass = "enterprise-multishare-rwx"

// BetaAPI is the served v1beta1 API multishare volumes are provisioned
// through.
type BetaAPI interface {
	ListInstances(context.Context, *betapb.ListInstancesRequest) (*betapb.ListInstancesResponse, error)
	GetInstance(context.Context, *betapb.GetInstanceRequest) (*betapb.Instance, error)
	CreateInstance(context.Context, *betapb.CreateInstanceRequest) (*longrunningpb.Operation, error)
	UpdateInstance(context.Context, *betapb.UpdateInstanceRequest) (*longrunningpb.Operation, error)
	DeleteInstance(context.Context, *betapb.DeleteInstanceRequest) (*longrunningpb.Operation, error)
	GetShare(context.Context, *betapb.GetShareRequest) (*betapb.Share, error)
	CreateShare(context.Context, *betapb.CreateShareRequest) (*longrunningpb.Operation, error)
	UpdateShare(context.Context, *betapb.UpdateShareRequest) (*longrunningpb.Operation, error)
	DeleteShare(context.Context, *betapb.DeleteShareRequest) (*longrunningpb.Operation, error)
}

// MultishareAPI is an API that also serves v1beta1.
type MultishareAPI interface {
	MultishareAPI() BetaAPI
}

const (
	multiStepGiB, multiMinGiB, multiMaxGiB = 256, 1024, 10240
	shareMinGiB                            = 10
)

// wait waits for op and answers its error.
func wait(ctx context.Context, api API, op *longrunningpb.Operation, err error) error {
	if err != nil {
		return err
	}
	api.WaitDone(ctx, op.GetName())
	done, err := api.Operation(op.GetName())
	if err != nil {
		return err
	}
	if e := done.GetError(); e != nil {
		return status.Error(codes.Code(e.GetCode()), e.GetMessage())
	}
	return nil
}

func roundUp(n, step int64) int64 { return (n + step - 1) / step * step }

// maxVolume is the class's max-volume-size in GiB (1 TiB by default).
func maxVolume(sc storageClass) (int64, error) {
	raw := sc.Parameters["max-volume-size"]
	if raw == "" {
		return 1024, nil
	}
	g, err := GiB(raw)
	if err != nil {
		return 0, fmt.Errorf("parameter max-volume-size: %v", err)
	}
	switch g {
	case 128, 256, 512, 1024:
		return g, nil
	}
	return 0, fmt.Errorf("parameter max-volume-size %q is not one of 128Gi, 256Gi, 512Gi and 1024Gi", raw)
}

// classLabel is the instances' storage class label value.
func classLabel(sc storageClass) string {
	if v := sc.Parameters["instance-storageclass-label"]; v != "" {
		return labelValue(v)
	}
	return labelValue(sc.Metadata.Name)
}

func (c *Controller) beta() (API, BetaAPI, error) {
	api := c.API()
	if api == nil {
		return nil, nil, fmt.Errorf("the Filestore API is not served")
	}
	m, ok := api.(MultishareAPI)
	if !ok {
		return nil, nil, fmt.Errorf("the Filestore API does not serve v1beta1")
	}
	return api, m.MultishareAPI(), nil
}

func multishare(sc storageClass) bool {
	return strings.EqualFold(sc.Parameters["multishare"], "true")
}

// provisionShare makes a claim's share on an instance of its class.
func (c *Controller) provisionShare(ctx context.Context, cl Cluster, sc storageClass, p pvc) error {
	api, beta, err := c.beta()
	if err != nil {
		return err
	}
	if t := strings.ToLower(sc.Parameters["tier"]); t != "" && t != "enterprise" {
		return fmt.Errorf("parameter tier %q: multishare volumes are Enterprise tier", sc.Parameters["tier"])
	}
	maxVol, err := maxVolume(sc)
	if err != nil {
		return err
	}
	req, err := GiB(p.Spec.Resources.Requests["storage"])
	if err != nil {
		return err
	}
	capGiB := max(req, shareMinGiB)
	if capGiB > maxVol {
		return fmt.Errorf("requested %d GiB is more than the class's max-volume-size of %d GiB", capGiB, maxVol)
	}
	location := cl.Location
	if i := strings.LastIndex(location, "-"); i > 0 && strings.Count(location, "-") == 2 {
		location = location[:i]
	}
	network := sc.Parameters["network"]
	if network == "" {
		network = cl.Network
	}
	label := classLabel(sc)
	parent := "projects/" + cl.Project + "/locations/" + location
	shareID := instanceID(p.Metadata.UID)
	mount := strings.ReplaceAll(shareID, "-", "_")
	maxShares := int64(multiMaxGiB) / maxVol

	// The class's instances; one with room for the share, grown if need be.
	list, err := beta.ListInstances(ctx, &betapb.ListInstancesRequest{Parent: parent})
	if err != nil {
		return err
	}
	var target *betapb.Instance
	var used int64
	for _, in := range list.GetInstances() {
		if !in.GetMultiShareEnabled() || in.GetLabels()["storage_gke_io_storage-class-id"] != label || in.GetMaxShareCount() != maxShares {
			continue
		}
		if sh, err := beta.GetShare(ctx, &betapb.GetShareRequest{Name: in.GetName() + "/shares/" + shareID}); err == nil {
			// Made by an earlier pass whose volume was not written.
			return c.shareVolume(ctx, cl, sc, p, in, sh, label, location)
		}
		if in.GetState() != betapb.Instance_READY {
			continue
		}
		u, n, err := c.sharesOf(ctx, beta, in)
		if err != nil {
			return err
		}
		if n < in.GetMaxShareCount() && u+capGiB <= multiMaxGiB {
			target, used = in, u
			break
		}
	}
	if target == nil {
		id := "fs-" + p.Metadata.UID
		if len(id) > 63 {
			id = id[:63]
		}
		op, err := beta.CreateInstance(ctx, &betapb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &betapb.Instance{
			Tier: betapb.Instance_ENTERPRISE, MultiShareEnabled: true, MaxShareCount: maxShares,
			CapacityGb: max(multiMinGiB, roundUp(capGiB, multiStepGiB)),
			Networks:   []*betapb.NetworkConfig{{Network: network, ReservedIpRange: sc.Parameters["reserved-ipv4-cidr"]}},
			Labels: map[string]string{"storage_gke_io_created-by": "filestore_csi_storage_gke_io",
				"storage_gke_io_storage-class-id": label},
			Description: "Filestore multishare instance of StorageClass " + sc.Metadata.Name}})
		if err := wait(ctx, api, op, err); err != nil {
			return fmt.Errorf("create a multishare instance: %w", err)
		}
		if target, err = beta.GetInstance(ctx, &betapb.GetInstanceRequest{Name: parent + "/instances/" + id}); err != nil {
			return err
		}
	} else if used+capGiB > target.GetCapacityGb() {
		if err := c.resizeInstance(ctx, api, beta, target, roundUp(used+capGiB, multiStepGiB)); err != nil {
			return err
		}
	}
	op, err := beta.CreateShare(ctx, &betapb.CreateShareRequest{Parent: target.GetName(), ShareId: shareID, Share: &betapb.Share{
		MountName: mount, CapacityGb: capGiB, Labels: map[string]string{
			"kubernetes_io_created-for_pv_name":       shareID,
			"kubernetes_io_created-for_pvc_name":      labelValue(p.Metadata.Name),
			"kubernetes_io_created-for_pvc_namespace": labelValue(p.Metadata.Namespace)}}})
	if err := wait(ctx, api, op, err); err != nil {
		return fmt.Errorf("create share %s on %s: %w", shareID, target.GetName(), err)
	}
	sh, err := beta.GetShare(ctx, &betapb.GetShareRequest{Name: target.GetName() + "/shares/" + shareID})
	if err != nil {
		return err
	}
	return c.shareVolume(ctx, cl, sc, p, target, sh, label, location)
}

// sharesOf are the capacity and the number of in's shares.
func (c *Controller) sharesOf(ctx context.Context, beta BetaAPI, in *betapb.Instance) (int64, int64, error) {
	lister, ok := beta.(interface {
		ListShares(context.Context, *betapb.ListSharesRequest) (*betapb.ListSharesResponse, error)
	})
	if !ok {
		return 0, 0, fmt.Errorf("the Filestore API does not list shares")
	}
	var used, n int64
	token := ""
	for {
		l, err := lister.ListShares(ctx, &betapb.ListSharesRequest{Parent: in.GetName(), PageToken: token})
		if err != nil {
			return 0, 0, err
		}
		for _, sh := range l.GetShares() {
			used += sh.GetCapacityGb()
			n++
		}
		if token = l.GetNextPageToken(); token == "" {
			return used, n, nil
		}
	}
}

func (c *Controller) resizeInstance(ctx context.Context, api API, beta BetaAPI, in *betapb.Instance, gib int64) error {
	gib = min(max(gib, multiMinGiB), multiMaxGiB)
	if gib == in.GetCapacityGb() {
		return nil
	}
	op, err := beta.UpdateInstance(ctx, &betapb.UpdateInstanceRequest{Instance: &betapb.Instance{Name: in.GetName(), CapacityGb: gib},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"capacity_gb"}}})
	if err := wait(ctx, api, op, err); err != nil {
		return fmt.Errorf("resize %s to %d GiB: %w", in.GetName(), gib, err)
	}
	c.logf("filestore CSI: resized multishare instance %s to %d GiB", in.GetName(), gib)
	return nil
}

// shareVolume writes the claim's volume for share sh of in.
func (c *Controller) shareVolume(ctx context.Context, cl Cluster, sc storageClass, p pvc, in *betapb.Instance, sh *betapb.Share, label, location string) error {
	if len(in.GetNetworks()) == 0 || len(in.GetNetworks()[0].GetIpAddresses()) == 0 {
		return fmt.Errorf("instance %s has no address yet", in.GetName())
	}
	handle := strings.Join([]string{"modeMultishare", label, cl.Project, location, lastSeg(in.GetName()), lastSeg(sh.GetName())}, "/")
	attrs := map[string]string{"ip": in.GetNetworks()[0].GetIpAddresses()[0], "volume": sh.GetMountName()}
	if err := c.writeVolume(ctx, cl, sc, p, lastSeg(sh.GetName()), sh.GetCapacityGb(), handle, attrs); err != nil {
		return err
	}
	c.logf("filestore CSI: provisioned %s/%s as share %s (%d GiB)", p.Metadata.Namespace, p.Metadata.Name, sh.GetName(), sh.GetCapacityGb())
	return nil
}

func lastSeg(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// multishareHandle parses a modeMultishare volume handle into its
// instance's and share's names.
func multishareHandle(handle string) (instance, share string, ok bool) {
	p := strings.Split(handle, "/")
	if len(p) != 6 || p[0] != "modeMultishare" {
		return "", "", false
	}
	instance = "projects/" + p[2] + "/locations/" + p[3] + "/instances/" + p[4]
	return instance, instance + "/shares/" + p[5], true
}

// deleteShare deletes a released volume's share; the instance shrinks to
// its remaining shares, or goes with its last share.
func (c *Controller) deleteShare(ctx context.Context, cl Cluster, pvName, handle string) error {
	instance, share, _ := multishareHandle(handle)
	api, beta, err := c.beta()
	if err != nil {
		return err
	}
	op, err := beta.DeleteShare(ctx, &betapb.DeleteShareRequest{Name: share})
	if status.Code(err) != codes.NotFound {
		if err := wait(ctx, api, op, err); err != nil {
			return fmt.Errorf("delete share %s: %w", share, err)
		}
	}
	in, err := beta.GetInstance(ctx, &betapb.GetInstanceRequest{Name: instance})
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return err
	default:
		used, n, err := c.sharesOf(ctx, beta, in)
		if err != nil {
			return err
		}
		if n == 0 {
			op, err := beta.DeleteInstance(ctx, &betapb.DeleteInstanceRequest{Name: instance})
			if err := wait(ctx, api, op, err); err != nil && status.Code(err) != codes.NotFound {
				return fmt.Errorf("delete instance %s: %w", instance, err)
			}
			c.logf("filestore CSI: deleted multishare instance %s with its last share", instance)
		} else if err := c.resizeInstance(ctx, api, beta, in, roundUp(used, multiStepGiB)); err != nil {
			return err
		}
	}
	if _, err := c.kubectl(ctx, cl, "", "delete", "pv", pvName, "--ignore-not-found", "--wait=false"); err != nil {
		return err
	}
	c.logf("filestore CSI: deleted %s and its share %s", pvName, share)
	return nil
}

// expandShare grows a bound claim's share, and its instance first if the
// share needs the room; it answers the share's capacity.
func (c *Controller) expandShare(ctx context.Context, handle string, want int64, sc storageClass) (int64, error) {
	instance, share, _ := multishareHandle(handle)
	api, beta, err := c.beta()
	if err != nil {
		return 0, err
	}
	if maxVol, err := maxVolume(sc); err == nil && want > maxVol {
		return 0, fmt.Errorf("requested %d GiB is more than the class's max-volume-size of %d GiB", want, maxVol)
	}
	sh, err := beta.GetShare(ctx, &betapb.GetShareRequest{Name: share})
	if err != nil {
		return 0, err
	}
	if sh.GetCapacityGb() >= want {
		return sh.GetCapacityGb(), nil
	}
	in, err := beta.GetInstance(ctx, &betapb.GetInstanceRequest{Name: instance})
	if err != nil {
		return 0, err
	}
	used, _, err := c.sharesOf(ctx, beta, in)
	if err != nil {
		return 0, err
	}
	if need := used - sh.GetCapacityGb() + want; need > in.GetCapacityGb() {
		if err := c.resizeInstance(ctx, api, beta, in, roundUp(need, multiStepGiB)); err != nil {
			return 0, err
		}
	}
	op, err := beta.UpdateShare(ctx, &betapb.UpdateShareRequest{Share: &betapb.Share{Name: share, CapacityGb: want},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"capacity_gb"}}})
	if err := wait(ctx, api, op, err); err != nil {
		return 0, fmt.Errorf("resize share %s to %d GiB: %w", share, want, err)
	}
	return want, nil
}

// GiBString is n GiB as a quantity.
func GiBString(n int64) string { return strconv.FormatInt(n, 10) + "Gi" }
