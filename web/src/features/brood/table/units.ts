export const GIB = 1073741824;

/** A decimal string of the API in a larger unit, at most one decimal. */
export const scaled = (value: string, unit = 1) => Math.round((Number(value) / unit) * 10) / 10;
