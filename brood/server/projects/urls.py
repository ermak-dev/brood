from django.urls import re_path

from brood.server.projects import views

urlpatterns = [
    re_path(r"^projects(?:/(?P<name>[^/]+))?$", views.ProjectView.as_view()),
    re_path(r"^projects/(?P<project>[^/]+)/actors(?:/(?P<name>[^/]+))?$",
            views.ActorView.as_view()),
]
