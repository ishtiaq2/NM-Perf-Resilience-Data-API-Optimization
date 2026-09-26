/**
 * Types for the DAS Web API contract (das-00-architecture-and-contract/contract).
 * Unknown extra fields may appear at any time and must be ignored.
 */

export type NodeStatus = 'online' | 'degraded' | 'offline';

export interface Heartbeat {
  status: 'ok' | 'degraded';
  server?: string;
  version?: string;
  bootId?: string;
  time?: number;
  eventLoop?: { lagP50Ms: number; lagP99Ms: number; lagMaxMs: number; utilization: number | null };
  services?: Record<string, 'ok' | 'degraded' | 'down' | 'unknown'>;
}

export interface Capabilities {
  api: 'das-v1';
  server?: string;
  version?: string;
  features: Partial<{
    etag: boolean;
    volatileDelta: boolean;
    spectrumLatest: boolean;
    spectrumBinary: boolean;
    spectrumDecimation: boolean;
    spectrumLongPoll: boolean;
    configOptimisticLocking: boolean;
    wsTelemetry: boolean;
    wsSpectrum: boolean;
  }>;
}

export interface Band {
  name: string;
  enabled: boolean;
  dlOutDbm: number;
  ulInDbm: number;
  dlGainDb: number;
  ulGainDb: number;
  vswr: number;
}

export interface Alarm {
  code: string;
  severity: 'minor' | 'major' | 'critical';
  since: number;
}

export interface NodeTelemetry {
  id: number;
  name: string;
  status: NodeStatus;
  type?: string;
  chain?: number;
  hop?: number;
  fw?: string;
  bootAt?: number;
  uptimeS?: number;
  temperatureC?: number;
  fanRpm?: number;
  psuVoltageV?: number;
  optical?: { rxDbm: number; txDbm: number; laserBiasMa: number };
  bands?: Band[];
  metrics?: Record<string, number>;
  alarms?: Alarm[];
}

export interface VolatileSnapshot {
  rev: number;
  generatedAt: number;
  nodeCount: number;
  nodes: Record<string, NodeTelemetry>;
}

export interface VolatileDelta {
  rev: number;
  base: number;
  generatedAt: number;
  changed: Record<string, NodeTelemetry>;
  removed: number[];
}

/** Legacy spectrum shape (shipped backend). */
export interface LegacySpectrum {
  sweepId: number;
  nodeId: number;
  port: number;
  timestamp: number;
  startHz: number;
  stopHz: number;
  rbwHz: number;
  points: { frequency: number; power: number }[];
}

export interface NodeConfig {
  name: string;
  dlGainDb: number;
  ulGainDb: number;
  bandsEnabled: Record<string, boolean>;
  tempAlarmC: number;
  notes: string;
}

export interface NodeConfigEnvelope {
  nodeId: number;
  version: number;
  updatedAt?: number;
  config: NodeConfig;
}

/** Telemetry WebSocket messages (asyncapi.yaml). */
export type TelemetryMessage =
  | { type: 'hello'; api: string; server: string; heartbeatMs: number; time: number }
  | ({ type: 'snapshot' } & VolatileSnapshot)
  | ({ type: 'delta' } & VolatileDelta)
  | { type: 'hb'; time: number; rev?: number }
  | { type: 'pong'; id: number; time: number }
  | { type: 'error'; code: string; message?: string };
