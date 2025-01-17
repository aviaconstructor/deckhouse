- name: d8.istio.versions
  rules:
    - alert: D8IstioDeprecatedIstioVersionInstalled
      annotations:
        description: |
          There is deprecated istio version `{{"{{$labels.version}}"}}` installed.
          Impact — version support will be removed in future deckhouse releases. The higher alert severity — the higher probability of support cancelling.
          Upgrading instructions — https://deckhouse.io/documentation/{{ $.Values.global.deckhouseVersion }}/modules/110-istio/examples.html#upgrading-istio.
        plk_markup_format: markdown
        plk_labels_as_annotations: pod,instance
        plk_protocol_version: "1"
        summary: There is deprecated istio version installed
      expr: |
        d8_istio_deprecated_version_installed{}
      for: 5m
      labels:
        severity_level: "{{"{{$labels.alert_severity}}"}}"
        tier: cluster
