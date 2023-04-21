# Copyright 2023 Wong Hoi Sing Edison <hswong3i@pantarei-design.com>
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

%global debug_package %{nil}

Name: containers-storage
Epoch: 100
Version: 1.47.0
Release: 1%{?dist}
Summary: Configuration files storage to github.com/containers
License: Apache-2.0
URL: https://github.com/containers/storage/tags
Source0: %{name}_%{version}.orig.tar.gz
BuildRequires: golang-1.20

%description
Configuration files and manpages shared by tools that are based on the
github.com/containers libraries, such as Buildah, CRI-O, Podman and
Skopeo.

%prep
%autosetup -T -c -n %{name}_%{version}-%{release}
tar -zx -f %{S:0} --strip-components=1 -C .

%build
set -ex && \
    export CGO_ENABLED=1 && \
    go build \
        -mod vendor -buildmode pie -v \
        -ldflags "-s -w" \
        -tags "netgo osusergo exclude_graphdriver_devicemapper exclude_graphdriver_btrfs containers_image_openpgp seccomp apparmor" \
        -o containers-storage ./cmd/containers-storage

%install
install -Dpm755 -d %{buildroot}%{_bindir}
install -Dpm755 -d %{buildroot}%{_datadir}/containers/
install -Dpm644 -t %{buildroot}%{_bindir} containers-storage
install -Dpm644 -t %{buildroot}%{_datadir}/containers/ storage.conf

%files
%license LICENSE
%dir %{_datadir}/containers/
%{_bindir}/containers-storage
%{_datadir}/containers/storage.conf

%changelog
