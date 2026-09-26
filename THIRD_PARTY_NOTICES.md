# Third-party notices

This file lists the third-party software included in the binaries and container
images built from this repository. The license texts of Go modules are included
in each module's source, which is retrieved by `go mod download`.

## Go modules compiled into the binaries

Generated with `go run github.com/google/go-licenses/v2@latest report ./cmd/...`
(regenerate after dependency changes).

| Module | License |
|---|---|
| github.com/beorn7/perks | MIT |
| github.com/cespare/xxhash | MIT |
| github.com/davecgh/go-spew | ISC |
| github.com/emicklei/go-restful | MIT |
| github.com/evanphx/json-patch | BSD-3-Clause |
| github.com/fsnotify/fsnotify | BSD-3-Clause |
| github.com/fxamacker/cbor | MIT |
| github.com/go-logr/logr | Apache-2.0 |
| github.com/go-logr/zapr | Apache-2.0 |
| github.com/go-openapi/jsonpointer | Apache-2.0 |
| github.com/go-openapi/jsonreference | Apache-2.0 |
| github.com/go-openapi/swag | Apache-2.0 |
| github.com/google/gnostic-models | Apache-2.0 |
| github.com/google/uuid | BSD-3-Clause |
| github.com/json-iterator/go | MIT |
| github.com/modern-go/concurrent | Apache-2.0 |
| github.com/modern-go/reflect2 | Apache-2.0 |
| github.com/munnerz/goautoneg | BSD-3-Clause |
| github.com/pmezard/go-difflib | BSD-3-Clause |
| github.com/prometheus/client_golang | Apache-2.0, BSD-3-Clause |
| github.com/prometheus/client_model | Apache-2.0 |
| github.com/prometheus/common | Apache-2.0 |
| github.com/prometheus/procfs | Apache-2.0 |
| github.com/spf13/pflag | BSD-3-Clause |
| github.com/vmware/govmomi | Apache-2.0, BSD-3-Clause |
| github.com/x448/float16 | MIT |
| go.uber.org/multierr | MIT |
| go.uber.org/zap | MIT |
| go.yaml.in/yaml | Apache-2.0, MIT |
| golang.org/x/net | BSD-3-Clause |
| golang.org/x/oauth2 | BSD-3-Clause |
| golang.org/x/sync | BSD-3-Clause |
| golang.org/x/sys | BSD-3-Clause |
| golang.org/x/term | BSD-3-Clause |
| golang.org/x/text | BSD-3-Clause |
| golang.org/x/time | BSD-3-Clause |
| gomodules.xyz/jsonpatch/v2 | Apache-2.0 |
| google.golang.org/protobuf | BSD-3-Clause |
| gopkg.in/evanphx | BSD-3-Clause |
| gopkg.in/inf.v0 | BSD-3-Clause |
| k8s.io/api | Apache-2.0 |
| k8s.io/apiextensions-apiserver | Apache-2.0 |
| k8s.io/apimachinery | Apache-2.0, BSD-3-Clause |
| k8s.io/client-go | Apache-2.0 |
| k8s.io/klog | Apache-2.0 |
| k8s.io/kube-openapi | Apache-2.0, BSD-3-Clause |
| k8s.io/utils | Apache-2.0, BSD-3-Clause |
| sigs.k8s.io/controller-runtime | Apache-2.0 |
| sigs.k8s.io/json | Apache-2.0, BSD-3-Clause |
| sigs.k8s.io/randfill | Apache-2.0 |
| sigs.k8s.io/structured-merge-diff | Apache-2.0 |
| sigs.k8s.io/yaml | Apache-2.0, BSD-3-Clause, MIT |

## Software in the worker container image

| Component | Version | License | Notes |
|---|---|---|---|
| qemu-img (Ubuntu package `qemu-utils`) | 1:8.2.2+ds-0ubuntu1.18 (pinned in `build/Dockerfile.worker`) | GPL-2.0-or-later (with some files under other GPL-compatible licenses) | Run as a separate subprocess; not linked with this project's code. Corresponding source: the Ubuntu `qemu` source package of the same version (`apt-get source qemu=1:8.2.2+ds-0ubuntu1.18` on Ubuntu 24.04, or https://launchpad.net/ubuntu/+source/qemu). The package's copyright file is shipped in the image at `/usr/share/doc/qemu-utils/copyright`. |
| Ubuntu 24.04 base image and packages (glibc, ca-certificates, qemu-utils dependencies) | pinned by digest | various (see `/usr/share/doc/*/copyright` in the image) | |

## Software in the controller container image

| Component | License |
|---|---|
| gcr.io/distroless/static-debian12 (pinned by digest) | various Debian package licenses; see https://github.com/GoogleContainerTools/distroless |

## Test-only material (not redistributed)

`hack/fetch-fixtures.sh` downloads `iotest-version3.vmdk.bz2` from the QEMU
repository (GPL-2.0-or-later) at test time, pinned by tag and SHA-256. It is not
committed to this repository or included in any image.

## NOTICE files of dependencies

Reproduced as required by section 4(d) of the Apache License 2.0.

### github.com/go-openapi/jsonpointer@v1.0.0

```
Copyright 2015-2025 go-swagger maintainers

// SPDX-FileCopyrightText: Copyright 2015-2025 go-swagger maintainers
// SPDX-License-Identifier: Apache-2.0

This software library, github.com/go-openapi/jsonpointer, includes software developed
by the go-swagger and go-openapi maintainers ("go-swagger maintainers").

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this software except in compliance with the License.

You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0.

This software is copied from, derived from, and inspired by other original software products.
It ships with copies of other software which license terms are recalled below.

The original software was authored on 25-02-2013 by sigu-399 (https://github.com/sigu-399, sigu.399@gmail.com).

github.com/sigu-399/jsonpointer
===========================

// SPDX-FileCopyrightText: Copyright 2013 sigu-399 ( https://github.com/sigu-399 )
// SPDX-License-Identifier: Apache-2.0

Copyright 2013 sigu-399 ( https://github.com/sigu-399 )

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

### github.com/go-openapi/jsonreference@v1.0.0

```
Copyright 2015-2025 go-swagger maintainers

// SPDX-FileCopyrightText: Copyright 2015-2025 go-swagger maintainers
// SPDX-License-Identifier: Apache-2.0

This software library, github.com/go-openapi/jsonreference, includes software developed
by the go-swagger and go-openapi maintainers ("go-swagger maintainers").

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this software except in compliance with the License.

You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0.

This software is copied from, derived from, and inspired by other original software products.
It ships with copies of other software which license terms are recalled below.

The original software was authored on 25-02-2013 by sigu-399 (https://github.com/sigu-399, sigu.399@gmail.com).

github.com/sigh-399/jsonreference
===========================

// SPDX-FileCopyrightText: Copyright 2013 sigu-399 ( https://github.com/sigu-399 )
// SPDX-License-Identifier: Apache-2.0

Copyright 2013 sigu-399 ( https://github.com/sigu-399 )

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

### github.com/prometheus/client_golang@v1.24.0

```
Prometheus instrumentation library for Go applications
Copyright 2012-2015 The Prometheus Authors

This product includes software developed at
SoundCloud Ltd. (http://soundcloud.com/).


The following components are included in this product:

perks - a fork of https://github.com/bmizerany/perks
https://github.com/beorn7/perks
Copyright 2013-2015 Blake Mizerany, Björn Rabenstein
See https://github.com/beorn7/perks/blob/master/README.md for license details.

Go support for Protocol Buffers - Google's data interchange format
http://github.com/golang/protobuf/
Copyright 2010 The Go Authors
See source code for license details.
```

### github.com/prometheus/client_model@v0.6.2

```
Data model artifacts for Prometheus.
Copyright 2012-2015 The Prometheus Authors

This product includes software developed at
SoundCloud Ltd. (http://soundcloud.com/).
```

### github.com/prometheus/common@v0.70.0

```
Common libraries shared by Prometheus Go components.
Copyright 2015 The Prometheus Authors

This product includes software developed at
SoundCloud Ltd. (http://soundcloud.com/).
```

### github.com/prometheus/procfs@v0.21.1

```
procfs provides functions to retrieve system, kernel and process
metrics from the pseudo-filesystem proc.

Copyright 2014-2015 The Prometheus Authors

This product includes software developed at
SoundCloud Ltd. (http://soundcloud.com/).
```

### go.yaml.in/yaml/v2@v2.4.4

```
Copyright 2011-2016 Canonical Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

### go.yaml.in/yaml/v3@v3.0.5

```
Copyright 2011-2016 Canonical Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

### sigs.k8s.io/randfill@v1.0.0

```
When donating the randfill project to the CNCF, we could not reach all the
gofuzz contributors to sign the CNCF CLA. As such, according to the CNCF rules
to donate a repository, we must add a NOTICE referencing section 7 of the CLA
with a list of developers who could not be reached.

`7. Should You wish to submit work that is not Your original creation, You may
submit it to the Foundation separately from any Contribution, identifying the
complete details of its source and of any license or other restriction
(including, but not limited to, related patents, trademarks, and license
agreements) of which you are personally aware, and conspicuously marking the
work as "Submitted on behalf of a third-party: [named here]".`

Submitted on behalf of a third-party: @dnephin (Daniel Nephin)
Submitted on behalf of a third-party: @AlekSi (Alexey Palazhchenko)
Submitted on behalf of a third-party: @bbigras (Bruno Bigras)
Submitted on behalf of a third-party: @samirkut (Samir)
Submitted on behalf of a third-party: @posener (Eyal Posener)
Submitted on behalf of a third-party: @Ashikpaul (Ashik Paul)
Submitted on behalf of a third-party: @kwongtailau (Kwongtai)
Submitted on behalf of a third-party: @ericcornelissen (Eric Cornelissen)
Submitted on behalf of a third-party: @eclipseo (Robert-André Mauchin)
Submitted on behalf of a third-party: @yanzhoupan (Andrew Pan)
Submitted on behalf of a third-party: @STRRL (Zhiqiang ZHOU)
Submitted on behalf of a third-party: @disconnect3d (Disconnect3d)
```
