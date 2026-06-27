# Home Manager module for dfetch. Exported as `homeManagerModules.default` from the flake
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.programs.dfetch;
  yamlFormat = pkgs.formats.yaml { };
in
{
  options.programs.dfetch = {
    enable = lib.mkEnableOption "dfetch, query and join data across any data source with SQL, on demand";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "dfetch.packages.\${system}.default";
      description = "The dfetch package to install.";
    };

    settings = lib.mkOption {
      inherit (yamlFormat) type;
      default = { };
      example = lib.literalExpression ''
        {
          sources = [
            {
              name = "jaeger";
              type = "jaeger";
              params.base_url = "http://jaeger.example.com:16686";
            }
          ];
          queries = [
            {
              name = "jaeger-errors";
              description = "Latest error spans for a service";
              params = [ "service" ];
              columns = [ "start_time" "operation_name" "status_message" "trace_id" ];
              sql = '''
                SELECT start_time, service_name, operation_name, status_message,
                       duration_ms, status_code, trace_id
                FROM jaeger.spans
                WHERE service_name = :service AND status_code = 'error'
                ORDER BY start_time DESC
                LIMIT 50
              ''';
            }
          ];
        }
      '';
      description = ''
        Configuration written verbatim as YAML to
        {file}`$XDG_CONFIG_HOME/dfetch/dfetch.yaml`. Each `sources` entry binds a
        SQL schema `name` to a connector `type`, with connector-specific
        `params`; `queries` holds saved queries. dfetch looks for `./dfetch.yaml`
        first, so a per-project config in the working directory still takes
        precedence at runtime.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    xdg.configFile."dfetch/dfetch.yaml" = lib.mkIf (cfg.settings != { }) {
      source = yamlFormat.generate "dfetch.yaml" cfg.settings;
    };
  };
}
