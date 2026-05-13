# DNS Controller

The DNS Controller orchsterates the configuration needed to setup the ATE routing.

We want to resolve requests for <actor UUID>.actors.resources.substrate.ate.dev to the router service address.

* Stub resolver mode: orchestrate running a CoreDNS instance with the UUID mapped to the router service address.

Cluster resources:

* Deployment `ate-system:dns`. Label: app=dns
* Service `ate-system:dns`. 
* ConfigMap `ate-system:dns`.

These are defined in manifests/ate-install/atenet-dns.yaml.

## Stub resolver mode

* Ensure stub resolver CoreDNS is running as:
  * Deployment `ate-system:dns`.
  * Service `ate-system:dns` pointing to the Deployment.

ConfigMap `ate-system:dns`:

```
# Match any 'A' query for a UUID pattern under actors.resources.substrate.ate.dev
    template IN A actors.resources.substrate.ate.dev {
        match "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\\.actors\\.resources\\.substrate\\.k8s\\.io\\.$"
        answer "{{ .Name }} 60 IN A <router service address>"
    }
```

## Integration

* CoreDNS: Update CoreDNS ConfigMap to add the stub resolver.
* GKE DNS: Update the GKE DNS ConfigMap to add the stub resolver.
