/**
 * `api.generated.ts` は openapi.yaml から自動生成される（手編集禁止）。
 * このファイルはそこに人間が使う名前を与えるだけの薄い層で、型の定義は行わない。
 * ここに独自の型を足し始めると API 契約が二重管理になるため、追加は必ず openapi.yaml 側で行う。
 */
import type { components } from "./api.generated";

type S = components["schemas"];

// ── リクエスト ────────────────────────────────────────────
export type SearchRequest = S["SearchRequest"];
export type EventInput = S["EventInput"];
export type VenueInput = S["VenueInput"];
export type PartyInput = S["PartyInput"];
export type SituationInput = S["SituationInput"];
export type DiningPreference = S["DiningPreference"];
export type LodgingPreference = S["LodgingPreference"];
export type ReturnTripPreference = S["ReturnTripPreference"];
export type PlanOptions = S["PlanOptions"];
export type ClickRequest = S["ClickRequest"];

// ── レスポンス ────────────────────────────────────────────
export type Plan = S["Plan"];
export type PlanId = S["PlanId"];
export type PlanStatus = S["PlanStatus"];
export type PlanEnvelope = S["PlanEnvelope"];
export type PlanCreatedEnvelope = S["PlanCreatedEnvelope"];
export type PlanSummary = S["PlanSummary"];
export type Alternatives = S["Alternatives"];
export type Warning = S["Warning"];
export type Meta = S["Meta"];
export type SourceStatus = S["SourceStatus"];
export type LlmMeta = S["LlmMeta"];
export type ErrorEnvelope = S["ErrorEnvelope"];
export type ErrorBody = S["ErrorBody"];

// ── LLM 由来（この 2 つだけが LLM の書いた文字列を含む） ──
export type PlanNarrative = S["PlanNarrative"];
export type SegmentNarrative = S["SegmentNarrative"];

// ── セグメント（判別可能ユニオン） ────────────────────────
export type Segment = S["Segment"];
export type SegmentType = S["SegmentType"];
export type BufferSegment = S["BufferSegment"];
export type MoveSegment = S["MoveSegment"];
export type DiningSegment = S["DiningSegment"];
export type LodgingSegment = S["LodgingSegment"];

export type BufferDetail = S["BufferDetail"];
export type MoveDetail = S["MoveDetail"];
export type DiningDetail = S["DiningDetail"];
export type LodgingDetail = S["LodgingDetail"];
export type TransitLeg = S["TransitLeg"];
export type Waypoint = S["Waypoint"];
export type LatLng = S["Location"];
export type Link = S["Link"];

/**
 * switch の網羅性チェック用。セグメント種別を openapi.yaml に追加すると、
 * 分岐を足し忘れた箇所がここでコンパイルエラーになる。
 */
export function assertNeverSegment(segment: never): never {
  throw new Error(`未対応のセグメント種別です: ${JSON.stringify(segment)}`);
}

/** SSE (`GET /v1/plans/{planId}/events`) が流すイベント。openapi.yaml の説明と対応。 */
export type PlanStreamEvent =
  | { event: "status"; data: { status: PlanStatus } }
  | { event: "sources"; data: SourceStatus }
  | { event: "plan"; data: Plan }
  | { event: "error"; data: ErrorBody };
