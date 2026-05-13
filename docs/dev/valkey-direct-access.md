# Accessing valkey directly

Sometimes you need to access the valkey instance directly, you can do this with one "simple" command:

1. `kubectl exec -n=ate-system -it valkey-cluster-0 -- valkey-cli -h valkey-cluster-service -c --tls --cacert /etc/valkey-ca/ca.crt --cer
t /run/servicedns.podcert.ate.dev/credential-bundle.pem --key /run/servicedns.podcert.ate.dev/credential-bundle.pem`
