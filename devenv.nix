{ pkgs, ... }:

{
  # https://devenv.sh/packages/
  packages = [
    pkgs.fluxcd
    pkgs.gnumake
    pkgs.jq
    pkgs.kind
    pkgs.kubectl
    pkgs.kubernetes-helm
    pkgs.shellcheck
    pkgs.yq-go
    pkgs.kubernetes-kcp
  ];

  # https://devenv.sh/languages/
  languages.go.enable = true;
  languages.go.version = "1.26.2";

  # See full reference at https://devenv.sh/reference/options/
}
