import httpx


def items_shape():
    # the sync service answers under /v1/shape
    return httpx.get(f"{ELECTRIC_URL}/v1/shape?table=items&offset=-1")
