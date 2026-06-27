# Installing with Nix

With flakes:

```nix
{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    dfetch = {
      url = "github:dmashuda/dfetch";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { nixpkgs, dfetch, ... }: {
    # use the `dfetch.overlays.default` Nixpkgs overlay to access `pkgs.dfetch`,
    # or alternatively reference the `dfetch.packages.${system}.default` package
    # directly in your NixOS, nix-darwin, or home-manager outputs
  };
}
```

## `home-manager`

The flake also outputs a home-manager module:

```nix
{
  imports = [ inputs.dfetch.homeManagerModules.default ];

  programs.dfetch = {
    enable = true;
    settings = {
      sources = [
        {
          name = "prod-traces";
          type = "jaeger";
          params.base_url = "http://jaeger.example.com:16686";
        }
      ];
      queries = [
        {
          name = "jaeger-errors";
          description = "Latest error spans for a service";
          params = [ "service" ];
          columns = [
            "start_time"
            "operation_name"
            "status_message"
            "trace_id"
          ];
          sql = /* sql */ ''
            SELECT start_time, service_name, operation_name, status_message,
                   duration_ms, status_code, trace_id
            FROM jaeger.spans
            WHERE service_name = :service AND status_code = 'error'
            ORDER BY start_time DESC
            LIMIT 50
          '';
        }
      ];
    };
  };
}
```
