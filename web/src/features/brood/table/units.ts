export const GIB = 1073741824;

/** An amount of the API, a number or a decimal string, in a larger unit, at
 * most one decimal. */
export const scaled = (value: number | string, unit = 1) =>
  Math.round((Number(value) / unit) * 10) / 10;
