from django.urls import re_path

from brood.server.projects import pools, views

urlpatterns = [
    re_path(r"^worker-pools(?:/(?P<name>[^/]+))?$", pools.PoolView.as_view()),
    re_path(r"^worker-pool-defaults$", pools.PoolDefaultsView.as_view()),
    re_path(r"^projects(?:/(?P<name>[^/]+))?$", views.ProjectView.as_view()),
    re_path(r"^projects/(?P<project>[^/]+)/actors(?:/(?P<name>[^/]+))?$",
            views.ActorView.as_view()),
    re_path(r"^projects/(?P<project>[^/]+)/templates(?:/(?P<name>[^/]+))?$",
            views.TemplateView.as_view()),
    re_path(r"^workers(?:/(?P<name>[^/]+))?$", views.WorkerView.as_view()),
    re_path(r"^policy/global$", views.PolicyView.as_view()),
    re_path(r"^policy/projects/(?P<project>[^/]+)$", views.PolicyView.as_view()),
]
