/**
 * 型レベルの回帰テスト。実行時には何もしない。
 * `tsc --noEmit` で「判別可能ユニオンが正しく絞り込めること」を検証する。
 * openapi.yaml の Segment 定義を壊すと、ここが最初に赤くなる。
 */
import { assertNeverSegment, type Link, type Segment } from "./api";

export function describeSegment(segment: Segment): string {
  switch (segment.type) {
    case "buffer":
      return segment.buffer.kind;
    case "move":
      return `${segment.move.travelMode} ${segment.move.distanceMeters}m`;
    case "dining":
      return segment.dining.name;
    case "lodging":
      return `${segment.lodging.name} / ${segment.lodging.plan.totalPriceJpy}円`;
    default:
      return assertNeverSegment(segment);
  }
}

/** dining / lodging のみ links を持つ。buffer / move には存在しない。 */
export function linksOf(segment: Segment): Link[] {
  return segment.type === "dining" || segment.type === "lodging"
    ? segment.links
    : [];
}
