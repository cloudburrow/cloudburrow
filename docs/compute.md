# Compute Engine VPC networking (`compute.googleapis.com` v1)

`--services compute` (#1936) serves the VPC networking part of the Compute Engine API in the CLI process: networks (auto and custom mode), subnetworks with their secondary ranges, firewall rules, routes, global and regional addresses, the global, regional and zonal operations, and the regions and zones. It is JSON under `/compute/v1/` on `--port-compute` (default `9031`), as Google's paths are, plus the batch endpoint `/batch/compute/v1` that gcloud sends its calls to. Compatibility status, method by method, is in [compatibility.md](compatibility.md#compute-engine-vpc-networking--computegoogleapiscom-v1).

VM instances, disks and images are not served (501); their design is a follow-up decision.

## What a network is here

Nothing carries traffic. A network, its subnets, its firewall rules and its routes are records, validated as Google validates them, that the other services check their own network fields against:

| Service | With `compute` | Without it |
|---|---|---|
| Memorystore for Redis | `authorized_network` must exist; a `reserved_ip_range` must not overlap a subnet or an allocated range of it (a range chosen for the instance is moved off them); with `PRIVATE_SERVICE_ACCESS`, a range name must be an allocated range of the network | any network and range |
| Redis Cluster, Memorystore for Valkey | each PSC network must exist | any network |
| Kubernetes Engine | the cluster's network must exist, and its subnetwork be that network's in the cluster's region | any network |
| Cloud SQL | `settings.ipConfiguration.privateNetwork` must exist, and `allocatedIpRange` be one of its allocated ranges | any network |

Every project starts with its `default` network, as a project with the Compute Engine API enabled does: auto mode, one `/20` subnet per region (the documented auto mode ranges, `10.128.0.0/20` in `us-central1`), the `default-allow-internal`, `-ssh`, `-rdp` and `-icmp` firewall rules, and the default route to the internet. Delete it and it is not made again.

Every insert, patch and delete completes at once and answers with a `DONE` operation (`compute#operation`), global, regional or zonal as Google scopes it; the clients' `Wait` returns at once.

External addresses are chosen from documentation ranges (`203.0.113.0/24` regional, `198.51.100.0/24` global), since no address here is reachable from the internet. Internal addresses are chosen from their subnet's primary range, skipping the four addresses Google reserves.

## Ports

Compute Engine is an API of its own, so it has a port of its own rather than sharing the Kubernetes Engine API's. The Kubernetes Engine port has long answered the one Compute call Terraform's `google_container_node_pool` makes (`instanceGroupManagers.get`, for a pool's size). With `compute` enabled, the Compute port answers it too, from the same node pools, and `cloudburrow terraform` points `compute_custom_endpoint` at the Compute port; without `compute`, nothing changes.

## Clients

```sh
cloudburrow up --services compute
eval "$(cloudburrow env)"            # CLOUDBURROW_COMPUTE_ENDPOINT, CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE
gcloud compute networks create vpc --subnet-mode custom
gcloud compute networks subnets create nodes --network vpc --region us-central1 --range 10.0.0.0/24 \
    --secondary-range pods=10.4.0.0/16
gcloud compute addresses create db --region us-central1 --subnet nodes
```

The Go client takes the endpoint with its scheme:

```go
c, err := compute.NewNetworksRESTClient(ctx,
    option.WithEndpoint("http://"+os.Getenv("CLOUDBURROW_COMPUTE_ENDPOINT")),
    option.WithoutAuthentication())
```

Terraform's `google_compute_network` and `google_compute_subnetwork` run through `cloudburrow terraform`.

## Console

The console's VPC network pages list and manage networks, subnets, firewall rules, routes and IP addresses, as Google's VPC network section does.
